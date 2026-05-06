/*
Copyright 2024.

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

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// argoCAPIFinalizer guards the ArgoCD cluster Secret so we can clean it up
// when the upstream CAPI Cluster CR is deleted. Without this, deleted CAPI
// Clusters leave orphan "unavailable" entries lingering in ArgoCD.
const argoCAPIFinalizer = "argo-capi.cloudnativesolutions.ro/finalizer"

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("cluster", req.NamespacedName)

	// retrieve the cluster object
	var cluster capi.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if errors.IsNotFound(err) {
			// Cluster has already been removed; nothing to do.
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch cluster")
		return ctrl.Result{}, err
	}

	// Connect to the management cluster (where the ArgoCD cluster Secret lives).
	mgmtClient, err := GetMgmtClusterConfig()
	if err != nil {
		log.Error(err, "unable to create client for management cluster")
		return ctrl.Result{}, err
	}

	secretName := fmt.Sprintf("cluster-%s", cluster.Name)

	// ── Deletion path ────────────────────────────────────────────────────
	// When the CAPI Cluster is being deleted, drop the ArgoCD cluster Secret
	// before letting the finalizer go so the workload cluster doesn't linger
	// in ArgoCD as "Unknown/Unavailable".
	if !cluster.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&cluster, argoCAPIFinalizer) {
			log.Info("cluster is being deleted; removing ArgoCD cluster secret", "secret", secretName)
			err := mgmtClient.CoreV1().Secrets("argocd").Delete(ctx, secretName, v1.DeleteOptions{})
			if err != nil && !errors.IsNotFound(err) {
				log.Error(err, "unable to delete ArgoCD cluster secret")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&cluster, argoCAPIFinalizer)
			if err := r.Update(ctx, &cluster); err != nil {
				log.Error(err, "unable to remove finalizer")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// ── Ensure finalizer is in place ─────────────────────────────────────
	// Set on first reconcile so the deletion path above will run.
	if !controllerutil.ContainsFinalizer(&cluster, argoCAPIFinalizer) {
		controllerutil.AddFinalizer(&cluster, argoCAPIFinalizer)
		if err := r.Update(ctx, &cluster); err != nil {
			log.Error(err, "unable to set finalizer")
			return ctrl.Result{}, err
		}
		// Re-queue with the freshly persisted object so the rest of the
		// reconcile sees the updated metadata.
		return ctrl.Result{Requeue: true}, nil
	}

	// if control plane is not ready, return and requeue
	if !cluster.Status.ControlPlaneReady {
		log.Info(fmt.Sprintf("cluster %s controlplane is not ready", cluster.Name))
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info(fmt.Sprintf("cluster %s controlplane is ready", cluster.Name))

	// create connection to target cluster
	targetClientset, targetConf, err := GetTargetClusterConfig(mgmtClient, cluster.Namespace, cluster.Name)
	if err != nil {
		log.Error(err, "unable to create client config for target cluster")
		return ctrl.Result{}, err
	}

	// create serviceaccount in target cluster
	svc, err := CreateServiceAccount(ctx, targetClientset, "argocd-manager")
	if err != nil {
		log.Error(err, "unable to create serviceaccount")
		return ctrl.Result{Requeue: true}, err
	}

	// create serviceaccount secret in target cluster
	svcscrt, err := CreateServiceAccountSecret(ctx, targetClientset, svc.Name)
	if err != nil {
		log.Error(err, "unable to create serviceaccount secret")
		return ctrl.Result{Requeue: true}, err
	}

	// retrieve the serviceaccount bearer token from target cluster
	token, err := GetServiceAccountBearerToken(ctx, targetClientset, *svcscrt)
	if err != nil {
		log.Error(err, "unable to get serviceaccount bearer token")
		return ctrl.Result{Requeue: true}, err
	}

	// create clusterrole in target cluster
	if _, err := CreateOrUpdateClusterRole(ctx, targetClientset); err != nil {
		log.Error(err, "unable to create or update clusterrole")
		return ctrl.Result{Requeue: true}, err
	}

	// create clusterrolebinding in target cluster
	if _, err := CreateOrUpdateClusterRoleBinding(ctx, targetClientset); err != nil {
		log.Error(err, "unable to create or update clusterrolebinding")
		return ctrl.Result{Requeue: true}, err
	}

	// Build the ArgoCD cluster config struct (matches ArgoCD's declarative
	// cluster setup format). See:
	// https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#clusters
	clusterConfig := ClusterConfig{
		BearerToken: string(token),
		TLSClientConfig: TLSClientConfig{
			Insecure: false,
			CAData:   targetConf.TLSClientConfig.CAData,
		},
	}
	data, err := json.Marshal(clusterConfig)
	if err != nil {
		log.Error(err, "unable to marshal cluster config")
		return ctrl.Result{}, err
	}

	// Propagate labels/annotations from the CAPI Cluster onto the ArgoCD
	// Secret so cluster-generator ApplicationSets can match by selector.
	labels := map[string]string{
		"argocd.argoproj.io/secret-type": "cluster",
	}
	for k, v := range cluster.GetLabels() {
		labels[k] = v
	}
	// Strip ArgoCD's app-of-apps tracking marker so the registered cluster
	// doesn't show up as a managed resource of the parent Application.
	delete(labels, "argocd.argoproj.io/instance")

	desired := &corev1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:      secretName,
			Namespace: "argocd",
			Labels:    labels,
		},
		Type: "Opaque",
		Data: map[string][]byte{
			"config": data,
			"name":   []byte(cluster.Name),
			"server": []byte(targetConf.Host),
		},
	}

	log.Info("reconciling ArgoCD cluster secret", "name", cluster.Name, "host", targetConf.Host)
	existing, err := mgmtClient.CoreV1().Secrets("argocd").Get(ctx, secretName, v1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		log.Error(err, "unable to read existing ArgoCD cluster secret")
		return ctrl.Result{}, err
	}

	if errors.IsNotFound(err) {
		if _, err := mgmtClient.CoreV1().Secrets("argocd").Create(ctx, desired, v1.CreateOptions{}); err != nil {
			log.Error(err, "unable to create ArgoCD cluster secret")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Preserve resourceVersion for the update.
	desired.ResourceVersion = existing.ResourceVersion
	if _, err := mgmtClient.CoreV1().Secrets("argocd").Update(ctx, desired, v1.UpdateOptions{}); err != nil {
		log.Error(err, "unable to update ArgoCD cluster secret")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capi.Cluster{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
