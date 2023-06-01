/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/apache/shardingsphere-on-cloud/shardingsphere-operator/api/v1alpha1"
	"github.com/apache/shardingsphere-on-cloud/shardingsphere-operator/pkg/kubernetes/service"
	"github.com/apache/shardingsphere-on-cloud/shardingsphere-operator/pkg/reconcile/storagenode/aws"
	"github.com/apache/shardingsphere-on-cloud/shardingsphere-operator/pkg/shardingsphere"

	"github.com/database-mesh/golang-sdk/aws/client/rds"
	dbmeshv1alpha1 "github.com/database-mesh/golang-sdk/kubernetes/api/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/strings/slices"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	StorageNodeControllerName = "storage-node-controller"
	FinalizerName             = "shardingsphere.apache.org/finalizer"

	AnnotationKeyRegisterStorageUnitEnabled = "shardingsphere.apache.org/register-storage-unit-enabled"
	AnnotationKeyComputeNodeNamespace       = "shardingsphere.apache.org/compute-node-namespace"
	AnnotationKeyComputeNodeName            = "shardingsphere.apache.org/compute-node-name"
	AnnotationKeyLogicDatabaseName          = "shardingsphere.apache.org/logic-database-name"

	ShardingSphereProtocolType = "proxy-frontend-database-protocol-type"
)

// StorageNodeReconciler is a controller for storage nodes
type StorageNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Log      logr.Logger
	Recorder record.EventRecorder
	AwsRDS   rds.RDS

	Service service.Service
}

// +kubebuilder:rbac:groups=shardingsphere.apache.org,resources=storagenodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=shardingsphere.apache.org,resources=storagenodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=shardingsphere.apache.org,resources=storagenodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.database-mesh.io,resources=databaseclasses,verbs=get;list;watch

// Reconcile handles main function of this controller
// nolint:gocognit
func (r *StorageNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("Reconciling StorageNode %s", req.NamespacedName))

	// get storage node
	node := &v1alpha1.StorageNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Get databaseClass with storageNode.Spec.DatabaseClassName
	databaseClass, err := r.getDatabaseClass(ctx, node)
	if err != nil {
		r.Log.Error(err, fmt.Sprintf("unable to fetch DatabaseClass %s", node.Spec.DatabaseClassName))
		return ctrl.Result{Requeue: true}, err
	}

	// finalize storage node
	if node.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent to registering our finalizer.
		if !slices.Contains(node.ObjectMeta.Finalizers, FinalizerName) {
			node.ObjectMeta.Finalizers = append(node.ObjectMeta.Finalizers, FinalizerName)
			if err := r.Update(ctx, node); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else if slices.Contains(node.ObjectMeta.Finalizers, FinalizerName) {
		return r.finalize(ctx, node, databaseClass)
	}

	// reconcile storage node
	return r.reconcile(ctx, databaseClass, node)
}

func (r *StorageNodeReconciler) finalize(ctx context.Context, node *v1alpha1.StorageNode, databaseClass *dbmeshv1alpha1.DatabaseClass) (ctrl.Result, error) {
	var err error
	switch node.Status.Phase {
	case v1alpha1.StorageNodePhaseReady, v1alpha1.StorageNodePhaseNotReady:
		// set storage node status to deleting
		node.Status.Phase = v1alpha1.StorageNodePhaseDeleting
	case v1alpha1.StorageNodePhaseDeleting:
		break
	case v1alpha1.StorageNodePhaseDeleteComplete:
		node.ObjectMeta.Finalizers = slices.Filter([]string{}, node.ObjectMeta.Finalizers, func(f string) bool {
			return f != FinalizerName
		})
		if err = r.Update(ctx, node); err != nil {
			r.Log.Error(err, "failed to remove finalizer")
		}
		return ctrl.Result{}, nil
	}

	// Try to unregister storage unit in shardingsphere.
	if err = r.unregisterStorageUnit(ctx, node); err != nil {
		r.Log.Error(err, "failed to delete storage unit")
		return ctrl.Result{RequeueAfter: defaultRequeueTime}, err
	}

	if err = r.deleteDatabaseCluster(ctx, node, databaseClass); err != nil {
		r.Log.Error(err, "failed to delete database cluster")
		return ctrl.Result{RequeueAfter: defaultRequeueTime}, err
	}

	desiredState := computeDesiredState(node.Status)

	if !reflect.DeepEqual(node.Status, desiredState) {
		node.Status = desiredState
		err := r.Status().Update(ctx, node)
		if err != nil {
			r.Log.Error(err, fmt.Sprintf("unable to update StorageNode %s/%s", node.GetNamespace(), node.GetName()))
			return ctrl.Result{Requeue: true}, err
		}
	}

	return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
}

func (r *StorageNodeReconciler) reconcile(ctx context.Context, dbClass *dbmeshv1alpha1.DatabaseClass, node *v1alpha1.StorageNode) (ctrl.Result, error) {
	// reconcile storage node with databaseClass
	switch dbClass.Spec.Provisioner {
	case dbmeshv1alpha1.ProvisionerAWSRDSInstance:
		if err := r.reconcileAwsRdsInstance(ctx, aws.NewRdsClient(r.AwsRDS), node, dbClass); err != nil {
			r.Log.Error(err, fmt.Sprintf("unable to reconcile AWS RDS Instance %s/%s, err:%s", node.GetNamespace(), node.GetName(), err.Error()))
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "Reconcile Failed", fmt.Sprintf("unable to reconcile AWS RDS Instance %s/%s, err:%s", node.GetNamespace(), node.GetName(), err.Error()))
		}
	case dbmeshv1alpha1.ProvisionerAWSAurora:
		if err := r.reconcileAwsAurora(ctx, aws.NewRdsClient(r.AwsRDS), node, dbClass); err != nil {
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "Reconcile Failed", fmt.Sprintf("unable to reconcile AWS Aurora %s/%s, err:%s", node.GetNamespace(), node.GetName(), err.Error()))
		}
	default:
		r.Recorder.Event(node, corev1.EventTypeWarning, "UnsupportedDatabaseProvisioner", fmt.Sprintf("unsupported database provisioner %s", dbClass.Spec.Provisioner))
		r.Log.Error(nil, fmt.Sprintf("unsupported database provisioner %s", dbClass.Spec.Provisioner))
	}

	// register storage unit if needed.
	if err := r.registerStorageUnit(ctx, node, dbClass); err != nil {
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "RegisterStorageUnitFailed", "unable to register storage unit %s/%s", node.GetNamespace(), node.GetName())
		return ctrl.Result{Requeue: true}, err
	}

	desiredState := computeDesiredState(node.Status)

	if !reflect.DeepEqual(node.Status, desiredState) {
		node.Status = desiredState
		err := r.Status().Update(ctx, node)
		if err != nil {
			r.Log.Error(err, fmt.Sprintf("unable to update StorageNode %s/%s", node.GetNamespace(), node.GetName()))
			return ctrl.Result{Requeue: true}, err
		}
	}

	return ctrl.Result{RequeueAfter: defaultRequeueTime}, nil
}

func (r *StorageNodeReconciler) getDatabaseClass(ctx context.Context, node *v1alpha1.StorageNode) (databaseClass *dbmeshv1alpha1.DatabaseClass, err error) {
	if node.Spec.DatabaseClassName == "" {
		r.Recorder.Event(node, corev1.EventTypeWarning, "DatabaseClassNameIsNil", "DatabaseClassName is nil")
		return nil, fmt.Errorf("DatabaseClassName is nil")
	}

	databaseClass = &dbmeshv1alpha1.DatabaseClass{}

	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.DatabaseClassName}, databaseClass); err != nil {
		r.Log.Error(err, fmt.Sprintf("unable to fetch DatabaseClass %s", node.Spec.DatabaseClassName))
		r.Recorder.Event(node, corev1.EventTypeWarning, "DatabaseClassNotFound", fmt.Sprintf("DatabaseClass %s not found", node.Spec.DatabaseClassName))
		return nil, err
	}

	// check provisioner
	// aws-like provisioner need aws rds client
	if databaseClass.Spec.Provisioner == dbmeshv1alpha1.ProvisionerAWSRDSInstance || databaseClass.Spec.Provisioner == dbmeshv1alpha1.ProvisionerAWSAurora {
		if r.AwsRDS == nil {
			r.Recorder.Event(node, corev1.EventTypeWarning, "AwsRdsClientIsNil", "aws rds client is nil, please check your aws credentials")
			return nil, fmt.Errorf("aws rds client is nil, please check your aws credentials")
		}
	}

	return databaseClass, nil
}

// nolint:gocritic
func computeDesiredState(status v1alpha1.StorageNodeStatus) v1alpha1.StorageNodeStatus {
	// Initialize a new status object based on the current state
	desiredState := status
	clusterStatus := status.Cluster.Status

	if status.Phase == v1alpha1.StorageNodePhaseDeleting {
		// If the storage node is being deleted, check if all instances are deleted.
		if len(status.Instances) == 0 {
			desiredState.Phase = v1alpha1.StorageNodePhaseDeleteComplete
		}
	} else {
		// If the storage node is not being deleted, check if all instances are ready.
		if (clusterStatus == "" || clusterStatus == "Ready") && allInstancesReady(status.Instances) {
			desiredState.Phase = v1alpha1.StorageNodePhaseReady
		} else {
			desiredState.Phase = v1alpha1.StorageNodePhaseNotReady
		}
	}

	desiredState.Conditions = computeNewConditions(desiredState, status, clusterStatus)

	return desiredState
}

// nolint:gocritic
func computeNewConditions(desiredState, status v1alpha1.StorageNodeStatus, clusterStatus string) v1alpha1.StorageNodeConditions {
	newSNConditions := status.Conditions

	// Update the cluster ready condition if the cluster status is not empty
	if clusterStatus != "" {
		if clusterStatus == "Ready" {
			newSNConditions.UpsertCondition(&v1alpha1.StorageNodeCondition{
				Type:           v1alpha1.StorageNodeConditionTypeClusterReady,
				Status:         corev1.ConditionTrue,
				LastUpdateTime: metav1.Now(),
				Reason:         "Cluster is ready",
			})
		} else {
			newSNConditions.UpsertCondition(&v1alpha1.StorageNodeCondition{
				Type:           v1alpha1.StorageNodeConditionTypeClusterReady,
				Status:         corev1.ConditionFalse,
				LastUpdateTime: metav1.Now(),
				Reason:         "Cluster is not ready",
			})
		}
	} else {
		newSNConditions.RemoveCondition(v1alpha1.StorageNodeConditionTypeClusterReady)
	}

	// Update the available condition based on the phase
	if desiredState.Phase == v1alpha1.StorageNodePhaseReady {
		newSNConditions.UpsertCondition(&v1alpha1.StorageNodeCondition{
			Type:           v1alpha1.StorageNodeConditionTypeAvailable,
			Status:         corev1.ConditionTrue,
			LastUpdateTime: metav1.Now(),
			Reason:         "All instances are ready",
		})
	} else {
		newSNConditions.UpsertCondition(&v1alpha1.StorageNodeCondition{
			Type:           v1alpha1.StorageNodeConditionTypeAvailable,
			Status:         corev1.ConditionFalse,
			LastUpdateTime: metav1.Now(),
			Reason:         "One or more instances are not ready",
		})
	}

	// Update the registered condition
	if status.Registered {
		newSNConditions.UpsertCondition(&v1alpha1.StorageNodeCondition{
			Type:           v1alpha1.StorageNodeConditionTypeRegistered,
			Status:         corev1.ConditionTrue,
			LastUpdateTime: metav1.Now(),
			Reason:         "StorageNode is registered",
		})
	} else {
		newSNConditions.UpsertCondition(&v1alpha1.StorageNodeCondition{
			Type:           v1alpha1.StorageNodeConditionTypeRegistered,
			Status:         corev1.ConditionFalse,
			LastUpdateTime: metav1.Now(),
			Reason:         "StorageNode is not registered",
		})
	}
	return newSNConditions
}

// allInstancesReady returns true if all instances are ready, false otherwise
func allInstancesReady(instances []v1alpha1.InstanceStatus) bool {
	if len(instances) == 0 {
		return false
	}

	for idx := range instances {
		instance := &instances[idx]
		if !(instance.Status == "Ready") {
			return false
		}
	}

	return true
}

func (r *StorageNodeReconciler) reconcileAwsRdsInstance(ctx context.Context, client aws.IRdsClient, node *v1alpha1.StorageNode, dbClass *dbmeshv1alpha1.DatabaseClass) error {
	instance, err := client.GetInstance(ctx, node)
	if err != nil {
		return err
	}

	if instance == nil && node.Status.Phase != v1alpha1.StorageNodePhaseDeleting {
		err = client.CreateInstance(ctx, node, dbClass.Spec.Parameters)
		if err != nil {
			return err
		}

		instance, err = client.GetInstance(ctx, node)
		if err != nil {
			return err
		}
	}

	if err := updateAWSRDSInstanceStatus(node, instance); err != nil {
		return fmt.Errorf("updateAWSRDSInstanceStatus failed: %w", err)
	}

	return nil
}

func updateAWSRDSInstanceStatus(node *v1alpha1.StorageNode, instance *rds.DescInstance) error {
	instances := make([]v1alpha1.InstanceStatus, 0)

	if instance == nil {
		node.Status.Instances = instances
		return nil
	}

	status := instance.DBInstanceStatus
	if status == rds.DBInstanceStatusAvailable {
		status = rds.DBInstanceStatusReady
	}

	instances = append(instances, v1alpha1.InstanceStatus{
		Endpoint: v1alpha1.Endpoint{
			Address: instance.Endpoint.Address,
			Port:    instance.Endpoint.Port,
		},
		Status: string(status),
	})

	node.Status.Instances = instances
	return nil
}

func (r *StorageNodeReconciler) reconcileAwsAurora(ctx context.Context, client aws.IRdsClient, node *v1alpha1.StorageNode, dbClass *dbmeshv1alpha1.DatabaseClass) error {
	// get instance
	aurora, err := client.GetAuroraCluster(ctx, node)
	if err != nil {
		return err
	}
	if aurora == nil {
		// create instance
		err = client.CreateAuroraCluster(ctx, node, dbClass.Spec.Parameters)
		if err != nil {
			return err
		}
	}
	// TODO: update storage node status
	newStatus, err := updateClusterStatus(ctx, node, client, aurora)
	if err != nil {
		return err
	}
	node.Status.Cluster = newStatus
	if err := r.Status().Update(ctx, node); err != nil {
		r.Log.Error(err, fmt.Sprintf("Failed to update cluster status for node %s/%s", node.GetNamespace(), node.GetName()))
	}
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "Reconcile", "Reconciled Aurora cluster %s, status is %s", aurora.DBClusterIdentifier, aurora.Status)

	return nil
}

func updateClusterStatus(ctx context.Context, node *v1alpha1.StorageNode, client aws.IRdsClient, cluster *rds.DescCluster) (v1alpha1.ClusterStatus, error) {
	clusterStatus := v1alpha1.ClusterStatus{
		PrimaryEndpoint: v1alpha1.Endpoint{
			Address: cluster.PrimaryEndpoint,
			Port:    cluster.Port,
		},
	}
	status := cluster.Status
	if status == "available" {
		status = "Ready"
	}
	clusterStatus.Status = status

	if len(cluster.ReadReplicaIdentifiers) == 0 {
		clusterStatus.ReaderEndpoints = []v1alpha1.Endpoint{}
		return clusterStatus, nil
	} else {

		for _, readident := range cluster.ReadReplicaIdentifiers {
			instance, err := client.GetInstanceByIdentifier(ctx, readident)
			if err != nil {
				return clusterStatus, err
			}

			clusterStatus.ReaderEndpoints = append(clusterStatus.ReaderEndpoints, v1alpha1.Endpoint{
				Address: instance.Endpoint.Address,
				Port:    instance.Endpoint.Port,
			})
		}
		return clusterStatus, nil
	}
}

// deleteDatabaseCluster
func (r *StorageNodeReconciler) deleteDatabaseCluster(ctx context.Context, node *v1alpha1.StorageNode, databaseClass *dbmeshv1alpha1.DatabaseClass) error {
	switch databaseClass.Spec.Provisioner {
	case dbmeshv1alpha1.ProvisionerAWSRDSInstance:
		if err := r.deleteAWSRDSInstance(ctx, aws.NewRdsClient(r.AwsRDS), node, databaseClass); err != nil {
			return fmt.Errorf("delete aws rds instance failed: %w", err)
		}
	case dbmeshv1alpha1.ProvisionerAWSAurora:
		if err := aws.NewRdsClient(r.AwsRDS).DeleteAuroraCluster(ctx, node, databaseClass); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported database provisioner %s", databaseClass.Spec.Provisioner)
	}
	return nil
}

func (r *StorageNodeReconciler) deleteAWSRDSInstance(ctx context.Context, client aws.IRdsClient, node *v1alpha1.StorageNode, databaseClass *dbmeshv1alpha1.DatabaseClass) error {
	instance, err := client.GetInstance(ctx, node)
	if err != nil {
		return err
	}

	if instance != nil && instance.DBInstanceStatus != rds.DBInstanceStatusDeleting {
		if err := client.DeleteInstance(ctx, node, databaseClass); err != nil {
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "DeleteFailed", "Failed to delete instance %s: %s", node.Annotations[dbmeshv1alpha1.AnnotationsInstanceIdentifier], err.Error())
			return err
		}
		r.Recorder.Event(node, corev1.EventTypeNormal, "Deleting", fmt.Sprintf("instance %s is deleting", node.Annotations[dbmeshv1alpha1.AnnotationsInstanceIdentifier]))
	}

	// update instance status
	if err := updateAWSRDSInstanceStatus(node, instance); err != nil {
		return fmt.Errorf("updateAWSRDSInstanceStatus failed: %w", err)
	}

	return nil
}

// registerStorageUnit
func (r *StorageNodeReconciler) registerStorageUnit(ctx context.Context, node *v1alpha1.StorageNode, dbClass *dbmeshv1alpha1.DatabaseClass) error {
	// if register storage unit is not enabled, return
	if node.Annotations[AnnotationKeyRegisterStorageUnitEnabled] != "true" {
		return nil
	}

	if err := r.validateComputeNodeAnnotations(node); err != nil {
		return err
	}

	// if storage unit is already registered, return
	if node.Status.Registered {
		return nil
	}

	// if node is not ready, return
	if node.Status.Phase != v1alpha1.StorageNodePhaseReady {
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "RegisterWaiting", "Waiting to register storage unit for node %s/%s: node is not ready", node.GetNamespace(), node.GetName())
		return nil
	}

	logicDBName := node.Annotations[AnnotationKeyLogicDatabaseName]
	dbName := node.Annotations[dbmeshv1alpha1.AnnotationsInstanceDBName]

	ssServer, err := r.getShardingsphereServer(ctx, node)
	if err != nil {
		return fmt.Errorf("getShardingsphereServer failed: %w", err)
	}

	defer ssServer.Close()

	if err := ssServer.CreateDatabase(logicDBName); err != nil {
		return fmt.Errorf("create database failed: %w", err)
	}
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "LogicDatabaseCreated", "LogicDatabase %s is created", logicDBName)

	// TODO add cluster

	ins := node.Status.Instances[0]
	host := ins.Endpoint.Address
	port := ins.Endpoint.Port
	username := node.Annotations[dbmeshv1alpha1.AnnotationsMasterUsername]
	if username == "" {
		username = dbClass.Spec.Parameters["masterUsername"]
	}
	password := node.Annotations[dbmeshv1alpha1.AnnotationsMasterUserPassword]
	if password == "" {
		password = dbClass.Spec.Parameters["masterUserPassword"]
	}

	// TODO how to set ds name?
	if err := ssServer.RegisterStorageUnit(logicDBName, "ds_0", host, uint(port), dbName, username, password); err != nil {
		return fmt.Errorf("register storage node failed: %w", err)
	}
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "StorageUnitRegistered", "StorageUnit %s:%d/%s is registered", host, port, dbName)

	node.Status.Registered = true
	return nil
}

func (r *StorageNodeReconciler) unregisterStorageUnit(ctx context.Context, node *v1alpha1.StorageNode) error {
	if !node.Status.Registered {
		return nil
	}
	if err := r.validateComputeNodeAnnotations(node); err != nil {
		return err
	}

	logicDBName := node.Annotations[AnnotationKeyLogicDatabaseName]

	ssServer, err := r.getShardingsphereServer(ctx, node)
	if err != nil {
		return fmt.Errorf("getShardingsphereServer failed: %w", err)
	}

	defer ssServer.Close()

	// TODO how to set ds name?
	if err := ssServer.UnRegisterStorageUnit(logicDBName, "ds_0"); err != nil {
		return fmt.Errorf("unregister storage unit failed: %w", err)
	}

	r.Recorder.Eventf(node, corev1.EventTypeNormal, "StorageUnitUnRegistered", "StorageUnit of node %s/%s is unregistered", node.GetNamespace(), node.GetName())

	node.Status.Registered = false
	return nil
}

func (r *StorageNodeReconciler) validateComputeNodeAnnotations(node *v1alpha1.StorageNode) error {
	requiredAnnos := []string{
		AnnotationKeyLogicDatabaseName,
		dbmeshv1alpha1.AnnotationsInstanceDBName,
		AnnotationKeyComputeNodeNamespace,
		AnnotationKeyComputeNodeName,
	}

	for _, anno := range requiredAnnos {
		if v, ok := node.Annotations[anno]; !ok || v == "" {
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "RegisterChecking", "Waiting to register storage unit for node %s/%s: annotation %s is required", node.GetNamespace(), node.GetName(), anno)
			return fmt.Errorf("annotation %s is required", anno)
		}
	}

	return nil
}

func (r *StorageNodeReconciler) getShardingsphereServer(ctx context.Context, node *v1alpha1.StorageNode) (shardingsphere.IServer, error) {
	var (
		driver, host, username, password string
		port                             uint
	)

	// get compute node
	cn := &v1alpha1.ComputeNode{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Name:      node.Annotations[AnnotationKeyComputeNodeName],
		Namespace: node.Annotations[AnnotationKeyComputeNodeNamespace],
	}, cn); err != nil {
		return nil, fmt.Errorf("get compute node failed: %w", err)
	}

	serverConf := cn.Spec.Bootstrap.ServerConfig

	driver, ok := serverConf.Props[ShardingSphereProtocolType]
	if !ok || driver == "" {
		driver = "mysql"
	}
	driver = strings.ToLower(driver)

	if len(serverConf.Authority.Users) == 0 {
		return nil, fmt.Errorf("no user in compute node %s/%s", cn.Namespace, cn.Name)
	}

	username = strings.Split(serverConf.Authority.Users[0].User, "@")[0]
	password = serverConf.Authority.Users[0].Password

	// get service of compute node
	svc, err := r.Service.GetByNamespacedName(ctx, types.NamespacedName{
		Name:      node.Annotations[AnnotationKeyComputeNodeName],
		Namespace: node.Annotations[AnnotationKeyComputeNodeNamespace],
	})

	if err != nil || svc == nil {
		return nil, fmt.Errorf("get service failed: %w", err)
	}

	host = fmt.Sprintf("%s.%s", svc.Name, svc.Namespace)

	port = uint(svc.Spec.Ports[0].Port)

	ssServer, err := shardingsphere.NewServer(driver, host, port, username, password)
	if err != nil {
		return nil, fmt.Errorf("new shardingsphere server failed: %w", err)
	}

	return ssServer, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *StorageNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.StorageNode{}).
		Complete(r)
}
