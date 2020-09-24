/*
Copyright 2020 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package composed

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runtimev1alpha1 "github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	runtimecomposed "github.com/crossplane/crossplane-runtime/pkg/resource/unstructured/composed"

	"github.com/crossplane/crossplane/apis/apiextensions/v1alpha1"
)

const (
	errUnmarshal  = "cannot unmarshal base template"
	errFmtPatch   = "cannot apply the patch at index %d"
	errGetSecret  = "cannot get connection secret of composed resource"
	errNamePrefix = "name prefix is not found in labels"
)

// Label keys.
const (
	LabelKeyNamePrefixForComposed = "crossplane.io/composite"
	LabelKeyClaimName             = "crossplane.io/claim-name"
	LabelKeyClaimNamespace        = "crossplane.io/claim-namespace"
)

// ConfigureFn is a function that implements Configurator interface.
type ConfigureFn func(cp resource.Composite, cd resource.Composed, t v1alpha1.ComposedTemplate) error

// Configure calls ConfigureFn.
func (c ConfigureFn) Configure(cp resource.Composite, cd resource.Composed, t v1alpha1.ComposedTemplate) error {
	return c(cp, cd, t)
}

// DefaultConfigurator configures the composed resource with given raw template
// and metadata information from composite resource.
type DefaultConfigurator struct{}

// Configure applies the raw template and sets name and generateName.
func (*DefaultConfigurator) Configure(cp resource.Composite, cd resource.Composed, t v1alpha1.ComposedTemplate) error {
	// Any existing name will be overwritten when we unmarshal the template. We
	// store it here so that we can reset it after unmarshalling.
	name := cd.GetName()
	namespace := cd.GetNamespace()
	// PD -  support for namespaced objects - use the claim namespace
	if namespace == "" {
		namespace = cp.GetLabels()[LabelKeyClaimNamespace]
	}
	if err := json.Unmarshal(t.Base.Raw, cd); err != nil {
		return errors.Wrap(err, errUnmarshal)
	}
	if cp.GetLabels()[LabelKeyNamePrefixForComposed] == "" {
		return errors.New(errNamePrefix)
	}
	// This label will be used if composed resource is yet another composite.
	meta.AddLabels(cd, map[string]string{
		LabelKeyNamePrefixForComposed: cp.GetLabels()[LabelKeyNamePrefixForComposed],
		LabelKeyClaimName:             cp.GetLabels()[LabelKeyClaimName],
		LabelKeyClaimNamespace:        cp.GetLabels()[LabelKeyClaimNamespace],
	})
	// Unmarshalling the template will overwrite any existing fields, so we must
	// restore the existing name, if any. We also set generate name in case we
	// haven't yet named this composed resource.
	cd.SetGenerateName(cp.GetLabels()[LabelKeyNamePrefixForComposed] + "-")
	cd.SetName(name)
	cd.SetNamespace(namespace)
	return nil
}

// OverlayFn is a function that implements OverlayApplicator interface.
type OverlayFn func(cp resource.Composite, cd resource.Composed, t v1alpha1.ComposedTemplate) error

// Overlay calls OverlayFn.
func (o OverlayFn) Overlay(cp resource.Composite, cd resource.Composed, t v1alpha1.ComposedTemplate) error {
	return o(cp, cd, t)
}

// DefaultOverlayApplicator applies patches to the composed resource using the
// values on Composite resource and field bindings in ComposedTemplate.
type DefaultOverlayApplicator struct{}

// Overlay applies patches to composed resource.
func (*DefaultOverlayApplicator) Overlay(cp resource.Composite, cd resource.Composed, t v1alpha1.ComposedTemplate) error {
	for i, p := range t.Patches {
		if err := p.Apply(cp, cd); err != nil {
			return errors.Wrapf(err, errFmtPatch, i)
		}
	}
	return nil
}

// FetchFn is a function that implements the ConnectionDetailsFetcher interface.
type FetchFn func(ctx context.Context, cd resource.Composed, t v1alpha1.ComposedTemplate) (managed.ConnectionDetails, error)

// Fetch calls FetchFn.
func (f FetchFn) Fetch(ctx context.Context, cd resource.Composed, t v1alpha1.ComposedTemplate) (managed.ConnectionDetails, error) {
	return f(ctx, cd, t)
}

// APIConnectionDetailsFetcher fetches the connection secret of given composed
// resource if it has a connection secret reference.
type APIConnectionDetailsFetcher struct {
	client client.Client
}

// Fetch returns the connection secret details of composed resource.
func (cdf *APIConnectionDetailsFetcher) Fetch(ctx context.Context, cd resource.Composed, t v1alpha1.ComposedTemplate) (managed.ConnectionDetails, error) {
	// PD -  support for custom connection secrets
	sref, err := getWriteConnectionSecretToReference(ctx, cd, t)
	if err != nil {
		return nil, err
	}
	if sref == nil {
		return nil, nil
	}

	conn := managed.ConnectionDetails{}

	// It's possible that the composed resource does want to write a
	// connection secret but has not yet. We presume this isn't an issue and
	// that we'll propagate any connection details during a future
	// iteration.
	s := &corev1.Secret{}
	nn := types.NamespacedName{Namespace: sref.Namespace, Name: sref.Name}
	if err := cdf.client.Get(ctx, nn, s); client.IgnoreNotFound(err) != nil {
		return nil, errors.Wrap(err, errGetSecret)
	}

	for _, d := range t.ConnectionDetails {
		if d.Name != nil && d.Value != nil {
			conn[*d.Name] = []byte(*d.Value)
			continue
		}

		if d.FromConnectionSecretKey == nil {
			continue
		}

		if len(s.Data[*d.FromConnectionSecretKey]) == 0 {
			continue
		}

		key := *d.FromConnectionSecretKey
		if d.Name != nil {
			key = *d.Name
		}

		conn[key] = s.Data[*d.FromConnectionSecretKey]
	}

	return conn, nil
}

// PD - gets the secret reference when a connection custom secret path is defined
func getWriteConnectionSecretToReference(ctx context.Context, cd resource.Composed, t v1alpha1.ComposedTemplate) (*runtimev1alpha1.SecretReference, error) {
	if t.ConnectionSecretRef == nil {
		return cd.GetWriteConnectionSecretToReference(), nil
	}

	u, ok := cd.(*runtimecomposed.Unstructured)
	if !ok {
		return nil, errors.New("composed resource has to be Unstructured type")
	}
	paved := fieldpath.Pave(u.UnstructuredContent())

	name, err := paved.GetValue(t.ConnectionSecretRef.NamePath)
	if err != nil {
		return nil, errors.New("Secret name not found at path: " + t.ConnectionSecretRef.NamePath)
	}
	namespace, err := paved.GetValue(t.ConnectionSecretRef.NamespacePath)
	if err != nil {
		return nil, errors.New("Secret namespace not found at path: " + t.ConnectionSecretRef.NamespacePath)
	}
	if name == "" || namespace == "" {
		return nil, nil
	}
	return &runtimev1alpha1.SecretReference{Name: name.(string), Namespace: namespace.(string)}, nil
}

// DefaultReadinessChecker is a readiness checker which returns whether the composed
// resource is ready or not.
type DefaultReadinessChecker struct{}

// IsReady returns whether the composed resource is ready.
func (*DefaultReadinessChecker) IsReady(_ context.Context, cd resource.Composed, t v1alpha1.ComposedTemplate) (bool, error) { // nolint:gocyclo
	// NOTE(muvaf): The cyclomatic complexity of this function comes from the
	// mandatory repetitiveness of the switch clause, which is not really complex
	// in reality. Though beware of adding additional complexity besides that.

	if len(t.ReadinessChecks) == 0 {
		return resource.IsConditionTrue(cd.GetCondition(runtimev1alpha1.TypeReady)), nil
	}
	// TODO(muvaf): We can probably get rid of resource.Composed interface and fake.Composed
	// structs and use *runtimecomposed.Unstructured everywhere including tests.
	u, ok := cd.(*runtimecomposed.Unstructured)
	if !ok {
		return false, errors.New("composed resource has to be Unstructured type")
	}
	paved := fieldpath.Pave(u.UnstructuredContent())

	for i, check := range t.ReadinessChecks {
		var ready bool
		switch check.Type {
		case v1alpha1.ReadinessCheckNonEmpty:
			_, err := paved.GetValue(check.FieldPath)
			if resource.Ignore(fieldpath.IsNotFound, err) != nil {
				return false, err
			}
			ready = !fieldpath.IsNotFound(err)
		case v1alpha1.ReadinessCheckMatchString:
			val, err := paved.GetString(check.FieldPath)
			if resource.Ignore(fieldpath.IsNotFound, err) != nil {
				return false, err
			}
			ready = !fieldpath.IsNotFound(err) && val == check.MatchString
		case v1alpha1.ReadinessCheckMatchInteger:
			val, err := paved.GetInteger(check.FieldPath)
			if err != nil {
				return false, err
			}
			ready = !fieldpath.IsNotFound(err) && val == check.MatchInteger
		default:
			return false, errors.New(fmt.Sprintf("readiness check at index %d: an unknown type is chosen", i))
		}
		if !ready {
			return false, nil
		}
	}
	return true, nil
}
