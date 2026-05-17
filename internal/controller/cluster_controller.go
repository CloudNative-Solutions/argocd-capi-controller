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
	"k8s.io/client-go/kubernetes"
	capi "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Labels we set on the ArgoCD cluster Secret. The cluster-name label lets us
// reverse-map a Secret back to its source CAPI Cluster during the startup
// orphan scan without parsing the Secret name.
const (
	argoCDClusterSecretType  = "argocd.argoproj.io/secret-type"
	argoCDClusterSecretValue = "cluster"
	capiClusterNameLabel     = "cluster.x-k8s.io/cluster-name"
)

// secretNameForCluster mirrors ArgoCD's `cluster-<name>` convention.
func secretNameForCluster(name string) string {
	return fmt.Sprintf("cluster-%s", name)
}

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

	mgmtClient, err := GetMgmtClusterConfig()
	if err != nil {
		log.Error(err, "unable to create client for management cluster")
		return ctrl.Result{}, err
	}

	// ── Cluster CR may already be gone ───────────────────────────────────
	// When a CAPI Cluster is deleted the informer still delivers a final
	// reconcile for its key — the Get below returns NotFound. We use that
	// as our cleanup trigger instead of a finalizer; the watch is reliable
	// while the controller is running, and the startup orphan scan in
	// ReconcileOrphanSecrets covers the down-during-delete case.
	var cluster capi.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, deleteArgoCDClusterSecret(ctx, mgmtClient, req.Name)
		}
		log.Error(err, "unable to fetch cluster")
		return ctrl.Result{}, err
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
	// `cluster.x-k8s.io/cluster-name` is set unconditionally so the startup
	// orphan scan can reverse-map Secret → Cluster without parsing names.
	labels := map[string]string{
		argoCDClusterSecretType: argoCDClusterSecretValue,
		capiClusterNameLabel:    cluster.Name,
	}
	for k, v := range cluster.GetLabels() {
		labels[k] = v
	}
	// Strip ArgoCD's app-of-apps tracking marker so the registered cluster
	// doesn't show up as a managed resource of the parent Application.
	delete(labels, "argocd.argoproj.io/instance")

	secretName := secretNameForCluster(cluster.Name)
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

// deleteArgoCDClusterSecret removes the `cluster-<name>` Secret from the
// argocd namespace if present. Used by both the event-driven cleanup path
// (Reconcile-NotFound) and the startup orphan scan.
func deleteArgoCDClusterSecret(ctx context.Context, mgmtClient kubernetes.Interface, clusterName string) error {
	log := log.FromContext(ctx)
	secretName := secretNameForCluster(clusterName)
	if err := mgmtClient.CoreV1().Secrets("argocd").Delete(ctx, secretName, v1.DeleteOptions{}); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		log.Error(err, "unable to delete ArgoCD cluster secret", "secret", secretName)
		return err
	}
	log.Info("deleted ArgoCD cluster secret", "secret", secretName)
	return nil
}

// ReconcileOrphanSecrets runs once at controller startup to garbage-collect
// ArgoCD cluster Secrets whose backing CAPI Cluster CR no longer exists.
// This closes the "controller was down during delete" gap that an
// event-driven cleanup alone can't cover, without us having to put a
// finalizer on a CR we don't own.
//
// Pass a direct (uncached) client — the manager's cache isn't populated yet
// when this is called.
func ReconcileOrphanSecrets(ctx context.Context, capiClient client.Client) error {
	log := log.FromContext(ctx).WithName("orphan-scan")

	mgmtClient, err := GetMgmtClusterConfig()
	if err != nil {
		return fmt.Errorf("management client: %w", err)
	}

	secrets, err := mgmtClient.CoreV1().Secrets("argocd").List(ctx, v1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s",
			argoCDClusterSecretType, argoCDClusterSecretValue, capiClusterNameLabel),
	})
	if err != nil {
		return fmt.Errorf("list argocd cluster secrets: %w", err)
	}

	var clusters capi.ClusterList
	if err := capiClient.List(ctx, &clusters); err != nil {
		return fmt.Errorf("list CAPI clusters: %w", err)
	}
	live := make(map[string]struct{}, len(clusters.Items))
	for _, c := range clusters.Items {
		live[c.Name] = struct{}{}
	}

	for i := range secrets.Items {
		s := &secrets.Items[i]
		name := s.Labels[capiClusterNameLabel]
		if name == "" {
			continue
		}
		if _, ok := live[name]; ok {
			continue
		}
		log.Info("garbage-collecting orphan ArgoCD cluster secret",
			"secret", s.Name, "cluster", name)
		if err := deleteArgoCDClusterSecret(ctx, mgmtClient, name); err != nil {
			// Log and continue; one bad orphan shouldn't block the rest.
			log.Error(err, "failed to delete orphan", "secret", s.Name)
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capi.Cluster{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
