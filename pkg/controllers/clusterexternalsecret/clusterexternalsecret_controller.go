/*
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

package clusterexternalsecret

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	esv1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	"github.com/external-secrets/external-secrets/pkg/controllers/clusterexternalsecret/cesmetrics"
	ctrlmetrics "github.com/external-secrets/external-secrets/pkg/controllers/metrics"
)

// ClusterExternalSecretReconciler reconciles a ClusterExternalSecret object.
type Reconciler struct {
	client.Client
	Log             logr.Logger
	Scheme          *runtime.Scheme
	RequeueInterval time.Duration
}

const (
	errGetCES               = "could not get ClusterExternalSecret"
	errPatchStatus          = "unable to patch status"
	errConvertLabelSelector = "unable to convert labelselector"
	errNamespaces           = "could not get namespaces from selector"
	errGetExistingES        = "could not get existing ExternalSecret"
	errCreatingOrUpdating   = "could not create or update ExternalSecret"
	errSetCtrlReference     = "could not set the controller owner reference"
	errSecretAlreadyExists  = "external secret already exists in namespace"
	errNamespacesFailed     = "one or more namespaces failed"
	errFailedToDelete       = "external secret in non matching namespace could not be deleted"
)

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ClusterExternalSecret", req.NamespacedName)

	resourceLabels := ctrlmetrics.RefineNonConditionMetricLabels(map[string]string{"name": req.Name, "namespace": req.Namespace})
	start := time.Now()

	externalSecretReconcileDuration := cesmetrics.GetGaugeVec(cesmetrics.ClusterExternalSecretReconcileDurationKey)
	defer func() { externalSecretReconcileDuration.With(resourceLabels).Set(float64(time.Since(start))) }()

	var clusterExternalSecret esv1beta1.ClusterExternalSecret

	err := r.Get(ctx, req.NamespacedName, &clusterExternalSecret)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		log.Error(err, errGetCES)
		return ctrl.Result{}, nil
	}

	p := client.MergeFrom(clusterExternalSecret.DeepCopy())
	defer r.deferPatch(ctx, log, &clusterExternalSecret, p)

	refreshInt := r.RequeueInterval
	if clusterExternalSecret.Spec.RefreshInterval != nil {
		refreshInt = clusterExternalSecret.Spec.RefreshInterval.Duration
	}

	labelSelector, err := metav1.LabelSelectorAsSelector(&clusterExternalSecret.Spec.NamespaceSelector)
	if err != nil {
		log.Error(err, errConvertLabelSelector)
		return ctrl.Result{RequeueAfter: refreshInt}, err
	}

	namespaceList := v1.NamespaceList{}
	err = r.List(ctx, &namespaceList, &client.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		log.Error(err, errNamespaces)
		return ctrl.Result{RequeueAfter: refreshInt}, err
	}

	esName := clusterExternalSecret.Spec.ExternalSecretName
	if esName == "" {
		esName = clusterExternalSecret.ObjectMeta.Name
	}

	failedNamespaces := r.removeOldNamespaces(ctx, namespaceList, esName, clusterExternalSecret.Status.ProvisionedNamespaces)
	provisionedNamespaces := []string{}

	for _, namespace := range namespaceList.Items {
		existingES, err := r.getExternalSecret(ctx, namespace.Name, esName)

		if result := checkForError(err, existingES); result != "" {
			log.Error(err, result)
			failedNamespaces[namespace.Name] = result
			continue
		}

		if result, err := r.resolveExternalSecret(ctx, &clusterExternalSecret, existingES, namespace, esName, clusterExternalSecret.Spec.ExternalSecretMetadata); err != nil {
			log.Error(err, result)
			failedNamespaces[namespace.Name] = result
			continue
		}

		provisionedNamespaces = append(provisionedNamespaces, namespace.ObjectMeta.Name)
	}

	condition := NewClusterExternalSecretCondition(failedNamespaces, &namespaceList)
	SetClusterExternalSecretCondition(&clusterExternalSecret, *condition)

	clusterExternalSecret.Status.FailedNamespaces = toNamespaceFailures(failedNamespaces)
	sort.Strings(provisionedNamespaces)
	clusterExternalSecret.Status.ProvisionedNamespaces = provisionedNamespaces

	return ctrl.Result{RequeueAfter: refreshInt}, nil
}

func (r *Reconciler) resolveExternalSecret(ctx context.Context, clusterExternalSecret *esv1beta1.ClusterExternalSecret, existingES *metav1.PartialObjectMetadata, namespace v1.Namespace, esName string, esMetadata esv1beta1.ExternalSecretMetadata) (string, error) {
	// this means the existing ES does not belong to us
	if err := controllerutil.SetControllerReference(clusterExternalSecret, existingES, r.Scheme); err != nil {
		return errSetCtrlReference, err
	}

	externalSecret := esv1beta1.ExternalSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        esName,
			Namespace:   namespace.Name,
			Labels:      esMetadata.Labels,
			Annotations: esMetadata.Annotations,
		},
		Spec: clusterExternalSecret.Spec.ExternalSecretSpec,
	}

	if err := controllerutil.SetControllerReference(clusterExternalSecret, &externalSecret, r.Scheme); err != nil {
		return errSetCtrlReference, err
	}

	mutateFunc := func() error {
		externalSecret.Spec = clusterExternalSecret.Spec.ExternalSecretSpec
		return nil
	}

	// An empty mutate func as nothing needs to happen currently
	if _, err := ctrl.CreateOrUpdate(ctx, r.Client, &externalSecret, mutateFunc); err != nil {
		return errCreatingOrUpdating, err
	}

	return "", nil
}

func (r *Reconciler) removeExternalSecret(ctx context.Context, esName, namespace string) (string, error) {
	existingES, err := r.getExternalSecret(ctx, namespace, esName)
	// If we can't find it then just leave
	if err != nil && apierrors.IsNotFound(err) {
		return "", nil
	}

	if result := checkForError(err, existingES); result != "" {
		return result, err
	}

	err = r.Delete(ctx, existingES, &client.DeleteOptions{})

	if err != nil {
		return errFailedToDelete, err
	}

	return "", nil
}

func (r *Reconciler) deferPatch(ctx context.Context, log logr.Logger, clusterExternalSecret *esv1beta1.ClusterExternalSecret, p client.Patch) {
	if err := r.Status().Patch(ctx, clusterExternalSecret, p); err != nil {
		log.Error(err, errPatchStatus)
	}
}

func (r *Reconciler) removeOldNamespaces(ctx context.Context, namespaceList v1.NamespaceList, esName string, provisionedNamespaces []string) map[string]string {
	failedNamespaces := map[string]string{}
	// Loop through existing namespaces first to make sure they still have our labels
	for _, namespace := range getRemovedNamespaces(namespaceList, provisionedNamespaces) {
		result, err := r.removeExternalSecret(ctx, esName, namespace)
		if err != nil {
			r.Log.Error(err, "unable to delete external-secret")
		}
		if result != "" {
			failedNamespaces[namespace] = result
		}
	}

	return failedNamespaces
}

func (r *Reconciler) getExternalSecret(ctx context.Context, namespace, name string) (*metav1.PartialObjectMetadata, error) {
	// Should not use esv1beta1.ExternalSecret since we specify builder.OnlyMetadata and cache only metadata
	metadata := metav1.PartialObjectMetadata{}
	metadata.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   esv1beta1.Group,
		Version: esv1beta1.Version,
		Kind:    esv1beta1.ExtSecretKind,
	})
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &metadata)
	return &metadata, err
}

func checkForError(getError error, existingES *metav1.PartialObjectMetadata) string {
	if getError != nil && !apierrors.IsNotFound(getError) {
		return errGetExistingES
	}

	// No one owns this resource so error out
	if !apierrors.IsNotFound(getError) && len(existingES.ObjectMeta.OwnerReferences) == 0 {
		return errSecretAlreadyExists
	}

	return ""
}

func getRemovedNamespaces(nsList v1.NamespaceList, provisionedNs []string) []string {
	var removedNamespaces []string

	nsSet := map[string]struct{}{}
	for i := range nsList.Items {
		nsSet[nsList.Items[i].Name] = struct{}{}
	}

	for _, ns := range provisionedNs {
		if _, ok := nsSet[ns]; !ok {
			removedNamespaces = append(removedNamespaces, ns)
		}
	}

	return removedNamespaces
}

func toNamespaceFailures(failedNamespaces map[string]string) []esv1beta1.ClusterExternalSecretNamespaceFailure {
	namespaceFailures := make([]esv1beta1.ClusterExternalSecretNamespaceFailure, len(failedNamespaces))

	i := 0
	for namespace, message := range failedNamespaces {
		namespaceFailures[i] = esv1beta1.ClusterExternalSecretNamespaceFailure{
			Namespace: namespace,
			Reason:    message,
		}
		i++
	}
	sort.Slice(namespaceFailures, func(i, j int) bool { return namespaceFailures[i].Namespace < namespaceFailures[j].Namespace })
	return namespaceFailures
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, opts controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(opts).
		For(&esv1beta1.ClusterExternalSecret{}).
		Owns(&esv1beta1.ExternalSecret{}, builder.OnlyMetadata).
		Watches(
			&v1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(r.findObjectsForNamespace),
			builder.WithPredicates(namespacePredicate()),
		).
		Complete(r)
}

func (r *Reconciler) findObjectsForNamespace(ctx context.Context, namespace client.Object) []reconcile.Request {
	namespaceLabels := labels.Set(namespace.GetLabels())

	// Avoid consuming too much memory
	const limit = 100
	var requests []reconcile.Request
	options := &client.ListOptions{Limit: limit}

	for {
		var clusterExternalSecrets esv1beta1.ClusterExternalSecretList
		if err := r.List(ctx, &clusterExternalSecrets, options); err != nil {
			r.Log.Error(err, errGetCES)
			return []reconcile.Request{}
		}

		for i := range clusterExternalSecrets.Items {
			clusterExternalSecret := &clusterExternalSecrets.Items[i]
			labelSelector, err := metav1.LabelSelectorAsSelector(&clusterExternalSecret.Spec.NamespaceSelector)
			if err != nil {
				r.Log.Error(err, errConvertLabelSelector)
				return []reconcile.Request{}
			}

			if labelSelector.Matches(namespaceLabels) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      clusterExternalSecret.GetName(),
						Namespace: clusterExternalSecret.GetNamespace(),
					},
				})
			}
		}

		if clusterExternalSecrets.Continue == "" {
			break
		}

		options = &client.ListOptions{
			Limit:    limit,
			Continue: clusterExternalSecrets.Continue,
		}
	}

	return requests
}

func namespacePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return !reflect.DeepEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels())
		},
		DeleteFunc: func(deleteEvent event.DeleteEvent) bool {
			return true
		},
	}
}
