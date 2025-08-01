// SPDX-FileCopyrightText: The RamenDR authors
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ocmworkv1 "open-cluster-management.io/api/work/v1"
	viewv1beta1 "open-cluster-management.io/multicloud-operators-subscription/pkg/apis/view/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	csiaddonsv1alpha1 "github.com/csi-addons/kubernetes-csi-addons/api/csiaddons/v1alpha1"
	"github.com/go-logr/logr"
	ramen "github.com/ramendr/ramen/api/v1alpha1"
	"github.com/ramendr/ramen/internal/controller/util"
)

// DRClusterReconciler reconciles a DRCluster object
type DRClusterReconciler struct {
	client.Client
	APIReader         client.Reader
	Log               logr.Logger
	Scheme            *runtime.Scheme
	MCVGetter         util.ManagedClusterViewGetter
	ObjectStoreGetter ObjectStoreGetter
	RateLimiter       *workqueue.TypedRateLimiter[reconcile.Request]
}

// DRCluster condition reasons
const (
	DRClusterConditionReasonInitializing = "Initializing"
	DRClusterConditionReasonFencing      = "Fencing"
	DRClusterConditionReasonUnfencing    = "Unfencing"
	DRClusterConditionReasonCleaning     = "Cleaning"
	DRClusterConditionReasonFenced       = "Fenced"
	DRClusterConditionReasonUnfenced     = "Unfenced"
	DRClusterConditionReasonClean        = "Clean"
	DRClusterConditionReasonValidated    = "Succeeded"

	DRClusterConditionReasonFenceError   = "FenceError"
	DRClusterConditionReasonUnfenceError = "UnfenceError"
	DRClusterConditionReasonCleanError   = "CleanError"

	DRClusterConditionReasonError        = "Error"
	DRClusterConditionReasonErrorUnknown = "UnknownError"
)

//nolint:gosec
const (
	StorageAnnotationSecretName      = "drcluster.ramendr.openshift.io/storage-secret-name"
	StorageAnnotationSecretNamespace = "drcluster.ramendr.openshift.io/storage-secret-namespace"
	StorageAnnotationClusterID       = "drcluster.ramendr.openshift.io/storage-clusterid"
	StorageAnnotationDriver          = "drcluster.ramendr.openshift.io/storage-driver"
)

const (
	DRClusterNameAnnotation = "drcluster.ramendr.openshift.io/drcluster-name"
)

const (
	NetworkFencePrefix = "network-fence"
)

// SetupWithManager sets up the controller with the Manager.
func (r *DRClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// ensure next line is not greater than 120 columns
	drpcMapFun := handler.EnqueueRequestsFromMapFunc(handler.MapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			drpc, ok := obj.(*ramen.DRPlacementControl)
			if !ok {
				return []reconcile.Request{}
			}

			ctrl.Log.Info(fmt.Sprintf("DRCluster: Filtering DRPC (%s/%s)", drpc.Name, drpc.Namespace))

			return filterDRPC(drpc)
		}))

	mwPred := ManifestWorkPredicateFunc()

	mwMapFun := handler.EnqueueRequestsFromMapFunc(handler.MapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			mw, ok := obj.(*ocmworkv1.ManifestWork)
			if !ok {
				return []reconcile.Request{}
			}

			ctrl.Log.Info(fmt.Sprintf("DRCluster: Filtering ManifestWork (%s/%s)", mw.Name, mw.Namespace))

			return filterDRClusterMW(mw)
		}))

	mcvPred := ManagedClusterViewPredicateFunc()

	mcvMapFun := handler.EnqueueRequestsFromMapFunc(handler.MapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			mcv, ok := obj.(*viewv1beta1.ManagedClusterView)
			if !ok {
				return []reconcile.Request{}
			}

			ctrl.Log.Info(fmt.Sprintf("DRCluster: Filtering MCV (%s/%s)", mcv.Name, mcv.Namespace))

			return filterDRClusterMCV(mcv)
		}))

	controller := ctrl.NewControllerManagedBy(mgr)
	if r.RateLimiter != nil {
		controller.WithOptions(ctrlcontroller.Options{
			RateLimiter: *r.RateLimiter,
		})
	}

	return controller.
		For(&ramen.DRCluster{}).
		Watches(&ramen.DRPlacementControl{}, drpcMapFun, builder.WithPredicates(drpcPred())).
		Watches(&ramen.DRPolicy{}, drPolicyEventHandler(), builder.WithPredicates(drPolicyPredicate())).
		Watches(&ocmworkv1.ManifestWork{}, mwMapFun, builder.WithPredicates(mwPred)).
		Watches(&viewv1beta1.ManagedClusterView{}, mcvMapFun, builder.WithPredicates(mcvPred)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.drClusterConfigMapMapFunc)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.drClusterSecretMapFunc),
			builder.WithPredicates(util.CreateOrDeleteOrResourceVersionUpdatePredicate{}),
		).
		Complete(r)
}

func (r *DRClusterReconciler) drClusterConfigMapMapFunc(
	ctx context.Context, configMap client.Object,
) []reconcile.Request {
	if configMap.GetName() != HubOperatorConfigMapName || configMap.GetNamespace() != RamenOperatorNamespace() {
		return []reconcile.Request{}
	}

	drcusters := &ramen.DRClusterList{}
	if err := r.Client.List(context.TODO(), drcusters); err != nil {
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, len(drcusters.Items))
	for i, drcluster := range drcusters.Items {
		requests[i].Name = drcluster.GetName()
	}

	return requests
}

func (r *DRClusterReconciler) drClusterSecretMapFunc(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != RamenOperatorNamespace() {
		return []reconcile.Request{}
	}

	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return []reconcile.Request{}
	}

	return filterDRClusterSecret(ctx, r.Client, secret)
}

// drpcPred watches for updates to the DRPC resource and checks if it requires an appropriate DRCluster reconcile
func drpcPred() predicate.Funcs {
	log := ctrl.Log.WithName("Predicate").WithName("DRPC")

	drpcPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			drpcOld, ok := e.ObjectOld.(*ramen.DRPlacementControl)
			if !ok {
				return false
			}

			drpcNew, ok := e.ObjectNew.(*ramen.DRPlacementControl)
			if !ok {
				return false
			}

			return DRPCUpdateOfInterest(drpcOld, drpcNew)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			log.Info("Processing DRPC delete event", "name", e.Object.GetName(), "namespace", e.Object.GetNamespace())

			return true
		},
	}

	return drpcPredicate
}

// DRPCUpdateOfInterest returns a bool, which is true if the update to DRPC requires further processing
func DRPCUpdateOfInterest(oldDRPC, newDRPC *ramen.DRPlacementControl) bool {
	log := ctrl.Log.WithName("Predicate").WithName("DRPC")

	// Ignore DRPC if it is not failing over
	if newDRPC.Spec.Action != ramen.ActionFailover {
		return false
	}

	// Process DRPC, if action was just changed to failover across old and new
	if oldDRPC.Spec.Action != ramen.ActionFailover {
		return true
	}

	// Process DRPC, if action was not changed, but failover cluster was
	if oldDRPC.Spec.FailoverCluster != newDRPC.Spec.FailoverCluster {
		log.Info("Processing DRPC failover cluster change event",
			"name", newDRPC.GetName(),
			"namespace", newDRPC.GetNamespace())

		return true
	}

	if condition := meta.FindStatusCondition(newDRPC.Status.Conditions, ramen.ConditionAvailable); condition != nil &&
		condition.Status == metav1.ConditionTrue &&
		condition.ObservedGeneration == newDRPC.Generation {
		// Process DRPC if it was just failed over, we are interested in updating MMode deactivation
		oldCondition := meta.FindStatusCondition(oldDRPC.Status.Conditions, ramen.ConditionAvailable)
		if oldCondition != nil && oldCondition.Status != metav1.ConditionTrue {
			log.Info("Processing DRPC failed over event",
				"name", newDRPC.GetName(),
				"namespace", newDRPC.GetNamespace())

			return true
		}

		log.Info("Ignoring DRPC failed over event",
			"name", newDRPC.GetName(),
			"namespace", newDRPC.GetNamespace())

		return false
	}

	return true
}

// filterDRPC relies on the predicate DRPCIpdateOfInterest to filter out any DRPC other than ones failing over, as a
// result the filter function just uses the failoverCluster value to start the appropriate DRCluster reconcile
func filterDRPC(drpc *ramen.DRPlacementControl) []ctrl.Request {
	if drpc.Spec.FailoverCluster == "" {
		return []ctrl.Request{}
	}

	return []ctrl.Request{
		reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name: drpc.Spec.FailoverCluster,
			},
		},
	}
}

func filterDRClusterMW(mw *ocmworkv1.ManifestWork) []ctrl.Request {
	if mw.Annotations[DRClusterNameAnnotation] == "" {
		return []ctrl.Request{}
	}

	return []ctrl.Request{
		reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name: mw.Annotations[DRClusterNameAnnotation],
			},
		},
	}
}

func filterDRClusterMCV(mcv *viewv1beta1.ManagedClusterView) []ctrl.Request {
	if mcv.Annotations[DRClusterNameAnnotation] == "" {
		return []ctrl.Request{}
	}

	return []ctrl.Request{
		reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name: mcv.Annotations[DRClusterNameAnnotation],
			},
		},
	}
}

func filterDRClusterSecret(ctx context.Context, reader client.Reader, secret *corev1.Secret) []ctrl.Request {
	log := ctrl.Log.WithName("filterDRClusterSecret").WithName("Secret")

	drclusters := &ramen.DRClusterList{}
	if err := reader.List(ctx, drclusters); err != nil {
		log.Error(err, "Failed to list DRClusters")

		return []reconcile.Request{}
	}

	requests := []reconcile.Request{}

	for i := range drclusters.Items {
		drcluster := &drclusters.Items[i]

		s3ProfileName := drcluster.Spec.S3ProfileName

		if s3ProfileName == NoS3StoreAvailable {
			continue
		}

		s3StoreProfile, err := GetRamenConfigS3StoreProfile(context.TODO(), reader, s3ProfileName)
		if err != nil {
			log.Info("Failed to filter secret", "secret", secret.GetName(), "drcluster", drcluster.Name, "reason", err.Error())

			continue
		}

		if secret.GetName() == s3StoreProfile.S3SecretRef.Name {
			requests = append(requests,
				reconcile.Request{
					NamespacedName: types.NamespacedName{Name: drcluster.GetName()},
				},
			)
		}
	}

	return requests
}

//nolint:lll
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=drclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=drclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=drclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=drplacementcontrols,verbs=get;list;watch
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=drpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=addon.open-cluster-management.io,resources=managedclusteraddons,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=work.open-cluster-management.io,resources=manifestworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=view.open-cluster-management.io,resources=managedclusterviews,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.open-cluster-management.io,resources=placementrules,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=placements,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=applicationsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=list;watch

func (r *DRClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// TODO: Validate managedCluster name? and also ensure it is not deleted!
	// TODO: Setup views for storage class and VRClass to read and report IDs
	log := r.Log.WithValues("drc", req.NamespacedName.Name, "rid", util.GetRID())
	log.Info("reconcile enter")

	defer log.Info("reconcile exit")

	drcluster := &ramen.DRCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, drcluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("get: %w", err))
	}

	manifestWorkUtil := &util.MWUtil{
		Client:          r.Client,
		APIReader:       r.APIReader,
		Ctx:             ctx,
		Log:             log,
		InstName:        drcluster.Name,
		TargetNamespace: "",
	}

	u := &drclusterInstance{
		ctx: ctx, object: drcluster, client: r.Client, log: log, reconciler: r,
		mwUtil: manifestWorkUtil, namespacedName: req.NamespacedName,
	}

	u.initializeStatus()

	if util.ResourceIsDeleted(drcluster) {
		return r.processDeletion(u)
	}

	return r.processCreateOrUpdate(u)
}

// processCreateOrUpdate of a DRCluster resource
// Handle fencing after just processing spec correctness as other checks below may fail, owing to cluster being
// potentially unreachable. Fencing is to request fencing this cluster using another, hence this cluster may fail
// other live validation checks, but we still need to process the fence request.
//
//nolint:cyclop
func (r DRClusterReconciler) processCreateOrUpdate(u *drclusterInstance) (ctrl.Result, error) {
	var requeue bool

	u.log.Info("create/update")

	_, ramenConfig, err := ConfigMapGet(u.ctx, r.APIReader)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("config map get: %w", u.validatedSetFalseAndUpdate("ConfigMapGetFailed", err))
	}

	if err := u.addLabelsAndFinalizers(); err != nil {
		return ctrl.Result{}, fmt.Errorf("finalizer add update: %w", u.validatedSetFalseAndUpdate("FinalizerAddFailed", err))
	}

	if err := drClusterDeploy(u, ramenConfig); err != nil {
		return ctrl.Result{}, fmt.Errorf("drclusters deploy: %w", u.validatedSetFalseAndUpdate("DrClustersDeployFailed", err))
	}

	if err = validateCIDRsFormat(u.object, u.log); err != nil {
		return ctrl.Result{}, fmt.Errorf("drclusters CIDRs validate: %w",
			u.validatedSetFalseAndUpdate(ReasonValidationFailed, err))
	}

	requeue, err = u.clusterFenceHandle()
	if err != nil {
		u.log.Info("Error during processing fencing", "error", err)
	}

	if reason, err := validateS3Profile(u.ctx, r.APIReader, r.ObjectStoreGetter, u.object, u.namespacedName.String(),
		u.log); err != nil {
		return ctrl.Result{}, fmt.Errorf("drclusters s3Profile validate: %w", u.validatedSetFalseAndUpdate(reason, err))
	}

	if err := u.getDRClusterDeployedStatus(u.object); err != nil {
		return ctrl.Result{}, fmt.Errorf("drclusters deploy status: %w",
			u.validatedSetFalseAndUpdate("DrClustersDeployStatusCheckFailed", err))
	}

	if err := u.ensureDRClusterConfig(); err != nil {
		return ctrl.Result{}, fmt.Errorf(
			"failed to ensure DRClusterConfig: %w",
			u.validatedSetFalseAndUpdate("DRClusterConfigInProgress", err),
		)
	}

	setDRClusterValidatedCondition(&u.object.Status.Conditions, u.object.Generation, "Validated the cluster")

	err = u.clusterMModeHandler()
	if err != nil {
		requeue = true

		u.log.Info("Error during processing maintenance modes", "error", err)
	}

	if err := u.statusUpdate(); err != nil {
		u.log.Info("failed to update status", "failure", err)
	}

	return ctrl.Result{Requeue: requeue || u.requeue}, nil
}

func (u *drclusterInstance) initializeStatus() {
	// Save a copy of the instance status to be used for the DRCluster status update comparison
	u.object.Status.DeepCopyInto(&u.savedInstanceStatus)

	if u.savedInstanceStatus.Conditions == nil {
		u.savedInstanceStatus.Conditions = []metav1.Condition{}
	}

	if u.object.Status.Conditions == nil {
		// Set the DRCluster conditions to unknown as nothing is known at this point
		msg := "Initializing DRCluster"
		setDRClusterInitialCondition(&u.object.Status.Conditions, u.object.Generation, msg)
		u.setDRClusterPhase(ramen.Starting)
	}
}

func (u *drclusterInstance) getDRClusterDeployedStatus(drcluster *ramen.DRCluster) error {
	mw, err := u.mwUtil.GetDrClusterManifestWork(drcluster.Name)
	if err != nil {
		return fmt.Errorf("error in fetching DRCluster ManifestWork %v", err)
	}

	if mw == nil {
		return fmt.Errorf("missing DRCluster ManifestWork resource %v", err)
	}

	deployed := util.IsManifestInAppliedState(mw)
	if !deployed {
		return fmt.Errorf("DRCluster ManifestWork is not in applied state")
	}

	return nil
}

func validateS3Profile(ctx context.Context, apiReader client.Reader,
	objectStoreGetter ObjectStoreGetter,
	drcluster *ramen.DRCluster, listKeyPrefix string, log logr.Logger,
) (string, error) {
	if drcluster.Spec.S3ProfileName != NoS3StoreAvailable {
		if reason, err := s3ProfileValidate(ctx, apiReader, objectStoreGetter,
			drcluster.Spec.S3ProfileName, listKeyPrefix, log); err != nil {
			return reason, err
		}
	}

	return "", nil
}

func s3ProfileValidate(ctx context.Context, apiReader client.Reader,
	objectStoreGetter ObjectStoreGetter, s3ProfileName, listKeyPrefix string,
	log logr.Logger,
) (string, error) {
	objectStore, _, err := objectStoreGetter.ObjectStore(
		ctx, apiReader, s3ProfileName, "drpolicy validation", log)
	if err != nil {
		return "s3ConnectionFailed", fmt.Errorf("%s: %w", s3ProfileName, err)
	}

	if _, err := objectStore.ListKeys(listKeyPrefix); err != nil {
		return "s3ListFailed", fmt.Errorf("%s: %w", s3ProfileName, err)
	}

	return "", nil
}

func validateCIDRsFormat(drcluster *ramen.DRCluster, log logr.Logger) error {
	// validate the CIDRs format
	invalidCidrs := []string{}

	for i := range drcluster.Spec.CIDRs {
		if _, _, err := net.ParseCIDR(drcluster.Spec.CIDRs[i]); err != nil {
			invalidCidrs = append(invalidCidrs, drcluster.Spec.CIDRs[i])

			log.Error(err, ReasonValidationFailed)
		}
	}

	if len(invalidCidrs) > 0 {
		return fmt.Errorf("invalid CIDRs specified %s", strings.Join(invalidCidrs, ", "))
	}

	return nil
}

func (r DRClusterReconciler) processDeletion(u *drclusterInstance) (ctrl.Result, error) {
	u.log.Info("delete")

	// Undeploy manifests
	if err := drClusterUndeploy(u.object, u.mwUtil, u.reconciler.MCVGetter, u.log); err != nil {
		return ctrl.Result{}, fmt.Errorf("drclusters undeploy: %w", err)
	}

	if u.object.Spec.ClusterFence == ramen.ClusterFenceStateFenced ||
		u.object.Spec.ClusterFence == ramen.ClusterFenceStateUnfenced {
		requeue, err := u.handleDeletion()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("cleanup update: %w", err)
		}

		if requeue {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	if err := u.finalizerRemove(); err != nil {
		return ctrl.Result{}, fmt.Errorf("finalizer remove update: %w", err)
	}

	return ctrl.Result{}, nil
}

type drclusterInstance struct {
	ctx                 context.Context
	object              *ramen.DRCluster
	client              client.Client
	log                 logr.Logger
	reconciler          *DRClusterReconciler
	savedInstanceStatus ramen.DRClusterStatus
	mwUtil              *util.MWUtil
	namespacedName      types.NamespacedName
	requeue             bool
}

func (u *drclusterInstance) validatedSetFalseAndUpdate(reason string, err error) error {
	if err1 := u.statusConditionSetAndUpdate(ramen.DRClusterValidated,
		metav1.ConditionFalse, reason, err.Error()); err1 != nil {
		return err1
	}

	return err
}

func (u *drclusterInstance) statusConditionSetAndUpdate(
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	conditions := &u.object.Status.Conditions

	util.GenericStatusConditionSet(u.object, conditions, conditionType, status, reason, message, u.log)

	return u.statusUpdate()
}

func (u *drclusterInstance) statusUpdate() error {
	if !reflect.DeepEqual(u.savedInstanceStatus, u.object.Status) {
		if err := u.client.Status().Update(u.ctx, u.object); err != nil {
			u.log.Info(fmt.Sprintf("Failed to update drCluster status (%s/%s/%v)",
				u.object.Name, u.object.Namespace, err))

			return fmt.Errorf("failed to update drCluster status (%s/%s)", u.object.Name, u.object.Namespace)
		}

		u.log.Info(fmt.Sprintf("Updated drCluster Status (%s/%s)", u.object.Name, u.object.Namespace))

		return nil
	}

	u.log.Info(fmt.Sprintf("Nothing to update (%s/%s)", u.object.Name, u.object.Namespace))

	return nil
}

const drClusterFinalizerName = "drclusters.ramendr.openshift.io/ramen"

func (u *drclusterInstance) addLabelsAndFinalizers() error {
	return util.NewResourceUpdater(u.object).
		AddLabel(util.OCMBackupLabelKey, util.OCMBackupLabelValue).
		AddFinalizer(drClusterFinalizerName).
		Update(u.ctx, u.client)
}

func (u *drclusterInstance) finalizerRemove() error {
	return util.NewResourceUpdater(u.object).
		RemoveFinalizer(drClusterFinalizerName).
		Update(u.ctx, u.client)
}

func (u *drclusterInstance) ensureDRClusterConfig() error {
	drcConfig, err := u.generateDRClusterConfig()
	if err != nil {
		return err
	}

	if err := u.mwUtil.CreateOrUpdateDRCConfigManifestWork(u.object.Name, *drcConfig); err != nil {
		return fmt.Errorf("failed to create or update DRClusterConfig manifest on cluster %s (%w)",
			u.object.GetName(), err)
	}

	if !u.mwUtil.IsManifestApplied(u.object.Name, util.MWTypeDRCConfig) {
		return fmt.Errorf("DRClusterConfig is not applied to cluster (%s)", u.object.Name)
	}

	return nil
}

//nolint:funlen
func (u *drclusterInstance) generateDRClusterConfig() (*ramen.DRClusterConfig, error) {
	mc, err := util.NewManagedClusterInstance(u.ctx, u.client, u.object.GetName())
	if err != nil {
		return nil, err
	}

	clusterID, err := mc.ClusterID()
	if err != nil {
		return nil, err
	}

	drcConfig := ramen.DRClusterConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DRClusterConfig",
			APIVersion: "ramendr.openshift.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: u.object.GetName(),
		},
		Spec: ramen.DRClusterConfigSpec{
			ClusterID: clusterID,
		},
	}

	util.AddLabel(&drcConfig, util.CreatedByRamenLabel, "true")

	drpolicies, err := util.GetAllDRPolicies(u.ctx, u.reconciler.APIReader)
	if err != nil {
		return nil, err
	}

	// Ensure that schedules are not duplicated by, storing them in "added" to avoid adding a duplicate schedule from
	// another DRPolicy
	added := map[string]bool{}

	for idx := range drpolicies.Items {
		if util.ResourceIsDeleted(&drpolicies.Items[idx]) {
			continue
		}

		if drpolicies.Items[idx].Spec.SchedulingInterval == "" {
			continue
		}

		if !util.DrpolicyContainsDrcluster(&drpolicies.Items[idx], u.object.GetName()) {
			continue
		}

		if exists, ok := added[drpolicies.Items[idx].Spec.SchedulingInterval]; !ok || !exists {
			drcConfig.Spec.ReplicationSchedules = append(
				drcConfig.Spec.ReplicationSchedules,
				drpolicies.Items[idx].Spec.SchedulingInterval)

			added[drpolicies.Items[idx].Spec.SchedulingInterval] = true

			u.log.Info(fmt.Sprintf("added %s", drpolicies.Items[idx].Spec.SchedulingInterval))
		}
	}

	return &drcConfig, nil
}

// TODO:
//
//  1. For now by default fenceStatus is ClusterFenceStateUnfenced.
//     However, we need to handle explicit unfencing operation to unfence
//     a fenced cluster below, by deleting the fencing CR created by
//     ramen.
//
//  2. How to differentiate between ClusterFenceStateUnfenced being
//     set because a manually fenced cluster is manually unfenced against the
//     requirement to unfence a cluster that has been fenced by ramen.
//
// 3) Handle Ramen driven fencing here
func (u *drclusterInstance) clusterFenceHandle() (bool, error) {
	switch u.object.Spec.ClusterFence {
	case ramen.ClusterFenceStateUnfenced:
		return u.clusterUnfence()

	case ramen.ClusterFenceStateManuallyFenced:
		setDRClusterFencedCondition(&u.object.Status.Conditions, u.object.Generation, "Cluster Manually fenced")
		u.setDRClusterPhase(ramen.Fenced)
		// no requeue is needed and no error as this is a manual fence
		return false, nil

	case ramen.ClusterFenceStateManuallyUnfenced:
		setDRClusterCleanCondition(&u.object.Status.Conditions, u.object.Generation,
			"Cluster Manually Unfenced and clean")
		u.setDRClusterPhase(ramen.Unfenced)
		// no requeue is needed and no error as this is a manual unfence
		return false, nil

	case ramen.ClusterFenceStateFenced:
		return u.clusterFence()

	default:
		// This is needed when a DRCluster is created fresh without any fencing related information.
		// That is cluster being clean without any NetworkFence CR. Or is it? What if someone just
		// edits the resource and removes the entire line that has fencing state? Should that be
		// treated as cluster being clean or unfence?
		setDRClusterCleanCondition(&u.object.Status.Conditions, u.object.Generation, "Cluster Clean")
		u.setDRClusterPhase(ramen.Available)

		return false, nil
	}
}

func (u *drclusterInstance) handleDeletion() (bool, error) {
	drpolicies, err := util.GetAllDRPolicies(u.ctx, u.reconciler.APIReader)
	if err != nil {
		return true, fmt.Errorf("getting all drpolicies failed: %w", err)
	}

	peerCluster, err := getPeerCluster(u.ctx, drpolicies, u.reconciler, u.object, u.log)
	if err != nil {
		return true, fmt.Errorf("failed to get the peer cluster for the cluster %s: %w",
			u.object.Name, err)
	}

	return u.cleanClusters([]ramen.DRCluster{*u.object, peerCluster})
}

func pruneNFClassViews(
	m util.ManagedClusterViewGetter,
	log logr.Logger,
	clusterName string,
	survivorClassNames []string,
) error {
	mcvList, err := m.ListNFClassMCVs(clusterName)
	if err != nil {
		return err
	}

	return pruneClassViews(m, log, clusterName, survivorClassNames, mcvList)
}

func getNFClassesFromCluster(
	u *drclusterInstance,
	m util.ManagedClusterViewGetter,
	drcConfig *ramen.DRClusterConfig,
	clusterName string,
) ([]*csiaddonsv1alpha1.NetworkFenceClass, error) {
	nfClasses := []*csiaddonsv1alpha1.NetworkFenceClass{}
	nfClassNames := drcConfig.Status.NetworkFenceClasses
	annotations := make(map[string]string)
	// annotations[AllDRPolicyAnnotation] = clusterName

	for _, nfClassName := range nfClassNames {
		nfClass, err := m.GetNFClassFromManagedCluster(nfClassName, clusterName, annotations)
		if err != nil {
			return []*csiaddonsv1alpha1.NetworkFenceClass{}, err
		}

		nfClasses = append(nfClasses, nfClass)
	}

	return nfClasses, pruneNFClassViews(m, u.log, clusterName, nfClassNames)
}

// findMatchingNFClasses returns NetworkFenceClass names that match the given StorageClasses
// based on provisioner and storage ID annotations. NetworkFenceClasses are returned only if:
// 1. NetworkFenceClass provisioner matches StorageClass provisioner
// 2. NetworkFenceClass storage ID annotation contains the StorageClass storage ID
// If no matching NetworkFenceClasses are found, returns a slice with an empty string for generic fencing
func (u *drclusterInstance) findMatchingNFClasses(
	networkFenceClasses []*csiaddonsv1alpha1.NetworkFenceClass, storageClasses []*storagev1.StorageClass,
) []string {
	nfClasses := []string{}

	for _, nfc := range networkFenceClasses {
		for _, sc := range storageClasses {
			storageID := sc.GetLabels()[StorageIDLabel]

			nfClassAnnoations, ok := nfc.GetAnnotations()[StorageIDLabel]
			if !ok {
				continue
			}

			if sc.Provisioner == nfc.Spec.Provisioner &&
				slices.Contains(strings.Split(nfClassAnnoations, ","), storageID) {
				nfClasses = append(nfClasses, nfc.GetName())
			}
		}
	}

	if len(nfClasses) == 0 {
		nfClasses = append(nfClasses, "")
	}

	return nfClasses
}

// getNFClassesFromDRClusterConfig retrieves the DRClusterConfig for the given DRCluster
// and extracts StorageClasses and NetworkFenceClass resources to process network fencing
func (u *drclusterInstance) getNFClassesFromDRClusterConfig(cluster *ramen.DRCluster,
) ([]string, error) {
	annotations := make(map[string]string)
	annotations[AllDRPolicyAnnotation] = cluster.GetName()

	drcConfig, err := u.reconciler.MCVGetter.GetDRClusterConfigFromManagedCluster(cluster.GetName(), annotations)
	if err != nil {
		return nil, err
	}

	nfClasses, err := getNFClassesFromCluster(u, u.reconciler.MCVGetter, drcConfig, cluster.GetName())
	if err != nil {
		return nil, err
	}

	storageClasses, err := GetSClassesFromCluster(u.log, u.reconciler.MCVGetter, drcConfig, cluster.GetName())
	if err != nil {
		return nil, err
	}

	return u.findMatchingNFClasses(nfClasses, storageClasses), nil
}

func (u *drclusterInstance) clusterFence() (bool, error) {
	// Ideally, here it should collect all the DRClusters available
	// in the cluster and then match the appropriate peer cluster
	// out of them by looking at the storage relationships. However,
	// currently, DRCluster does not contain the storage relationship
	// identity. Until that capability is not available, the alternate
	// way is to collect all the DRPolicies and out of them choose the
	// cluster whose region is same is current DRCluster's region.
	// And that matching cluster is chosen as the peer cluster where
	// the fencing resource is created to fence off this cluster.
	drpolicies, err := util.GetAllDRPolicies(u.ctx, u.reconciler.APIReader)
	if err != nil {
		return true, fmt.Errorf("getting all drpolicies failed: %w", err)
	}

	peerCluster, err := getPeerCluster(u.ctx, drpolicies, u.reconciler, u.object, u.log)
	if err != nil {
		return true, fmt.Errorf("failed to get the peer cluster for the cluster %s: %w",
			u.object.Name, err)
	}

	nfClasses, err := u.getNFClassesFromDRClusterConfig(&peerCluster)
	if err != nil {
		return true, fmt.Errorf("faled to get NetworkFenceClasses: %w", err)
	}

	for _, nfClass := range nfClasses {
		reque, err := u.fenceClusterOnCluster(&peerCluster, nfClass)
		if err != nil {
			return reque, err
		}
	}

	return false, nil
}

//nolint:cyclop
func (u *drclusterInstance) clusterUnfence() (bool, error) {
	// Ideally, here it should collect all the DRClusters available
	// in the cluster and then match the appropriate peer cluster
	// out of them by looking at the storage relationships. However,
	// currently, DRCluster does not contain the storage relationship
	// identity. Until that capability is not available, the alternate
	// way is to collect all the DRPolicies and out of them choose the
	// cluster whose region is same is current DRCluster's region.
	// And that matching cluster is chosen as the peer cluster where
	// the fencing resource is created to fence off this cluster.
	drpolicies, err := util.GetAllDRPolicies(u.ctx, u.reconciler.APIReader)
	if err != nil {
		return true, fmt.Errorf("getting all drpolicies failed: %w", err)
	}

	peerCluster, err := getPeerCluster(u.ctx, drpolicies, u.reconciler, u.object,
		u.log)
	if err != nil {
		return true, fmt.Errorf("failed to get the peer cluster for the cluster %s: %w",
			u.object.Name, err)
	}

	processUnfence := func(networkFenceClassName string) (bool, error) {
		requeue, err := u.unfenceClusterOnCluster(&peerCluster, networkFenceClassName)
		if err != nil {
			return requeue, fmt.Errorf("unfence operation to unfence cluster %s on cluster %s failed: %w",
				u.object.Name, peerCluster.Name, err)
		}

		if requeue {
			u.log.Info("requing as cluster unfence operation is not complete")

			return requeue, nil
		}

		return false, nil
	}

	nfClasses, err := u.getNFClassesFromDRClusterConfig(&peerCluster)
	if err != nil {
		return true, fmt.Errorf("faled to get NetworkFenceClasses: %w", err)
	}

	for _, nfClass := range nfClasses {
		requeue, err := processUnfence(nfClass)
		if requeue || err != nil {
			return requeue, err
		}
	}

	// once this cluster is unfenced. Clean the fencing resource.
	return u.cleanClusters([]ramen.DRCluster{*u.object, peerCluster})
}

// if the fencing CR (via MCV) exists; then
//
//	if the status of fencing CR shows fenced
//	   return dontRequeue, nil
//	else
//	   return requeue, error
//	endif
//
// else
//
//	Create the fencing CR MW with Fenced state
//	return requeue, nil
//
// endif
func (u *drclusterInstance) fenceClusterOnCluster(peerCluster *ramen.DRCluster,
	networkFenceClassName string,
) (bool, error) {
	if !u.isFencingOrFenced() {
		u.log.Info(fmt.Sprintf("initiating the cluster fence from the cluster %s", peerCluster.Name))

		if err := u.createNFManifestWork(u.object, peerCluster, u.log, networkFenceClassName); err != nil {
			setDRClusterFencingFailedCondition(&u.object.Status.Conditions, u.object.Generation,
				fmt.Sprintf("NetworkFence ManifestWork creation failed: %v", err))

			u.log.Info(fmt.Sprintf("Failed to generate NetworkFence MW on cluster %s to unfence %s",
				peerCluster.Name, u.object.Name))

			return true, fmt.Errorf("failed to create the NetworkFence MW on cluster %s to fence %s: %w",
				peerCluster.Name, u.object.Name, err)
		}

		setDRClusterFencingCondition(&u.object.Status.Conditions, u.object.Generation,
			"ManifestWork for NetworkFence fence operation created")
		u.setDRClusterPhase(ramen.Fencing)
		// just created fencing resource. Requeue and then check.
		return true, nil
	}

	annotations := make(map[string]string)
	annotations[DRClusterNameAnnotation] = u.object.Name

	nf, err := u.reconciler.MCVGetter.GetNFFromManagedCluster(u.object.Name,
		u.object.Namespace, peerCluster.Name, annotations)
	if err != nil {
		// dont update the status or conditions. Return requeue, nil as
		// this indicates that NetworkFence resource might have been not yet
		// created in the manged cluster or MCV for it might not have been
		// created yet. This assumption is because, drCluster does not delete
		// the NetworkFence resource as part of fencing.
		return true, fmt.Errorf("failed to get NetworkFence using MCV (error: %w)", err)
	}

	if nf.Spec.FenceState != csiaddonsv1alpha1.FenceState(u.object.Spec.ClusterFence) {
		return true, fmt.Errorf("fence state in the NetworkFence resource is not changed to %v yet",
			u.object.Spec.ClusterFence)
	}

	if nf.Status.Result != csiaddonsv1alpha1.FencingOperationResultSucceeded {
		setDRClusterFencingFailedCondition(&u.object.Status.Conditions, u.object.Generation,
			"fencing operation not successful")

		u.log.Info("Fencing operation not successful", "cluster", u.object.Name)

		return true, fmt.Errorf("fencing operation result not successful")
	}

	setDRClusterFencedCondition(&u.object.Status.Conditions, u.object.Generation,
		"Cluster successfully fenced")
	u.advanceToNextPhase()

	return false, nil
}

// if the fencing CR (via MCV) exist; then
//
//	if the status of fencing CR shows unfenced
//	   return dontRequeue, nil
//	else
//	   return requeue, error
//	endif
//
// else
//
//	Create the fencing CR MW with Unfenced state
//	return requeue, nil
//
// endif
func (u *drclusterInstance) unfenceClusterOnCluster(peerCluster *ramen.DRCluster,
	networkFenceClassName string,
) (bool, error) {
	if !u.isUnfencingOrUnfenced() {
		u.log.Info(fmt.Sprintf("initiating the cluster unfence from the cluster %s", peerCluster.Name))

		if err := u.createNFManifestWork(u.object, peerCluster, u.log, networkFenceClassName); err != nil {
			setDRClusterUnfencingFailedCondition(&u.object.Status.Conditions, u.object.Generation,
				"NeworkFence ManifestWork for unfence failed")

			u.log.Info(fmt.Sprintf("Failed to generate NetworkFence MW on cluster %s to unfence %s",
				peerCluster.Name, u.object.Name))

			return true, fmt.Errorf("failed to generate NetworkFence MW on cluster %s to unfence %s",
				peerCluster.Name, u.object.Name)
		}

		setDRClusterUnfencingCondition(&u.object.Status.Conditions, u.object.Generation,
			"ManifestWork for NetworkFence unfence operation created")
		u.setDRClusterPhase(ramen.Unfencing)

		// just created NetworkFence resource to unfence. Requeue and then check.
		return true, nil
	}

	annotations := make(map[string]string)
	annotations[DRClusterNameAnnotation] = u.object.Name

	nf, err := u.reconciler.MCVGetter.GetNFFromManagedCluster(u.object.Name,
		u.object.Namespace, peerCluster.Name, annotations)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return u.requeueIfNFMWExists(peerCluster)
		}

		return true, fmt.Errorf("failed to get NetworkFence using MCV (error: %w", err)
	}

	if nf.Spec.FenceState != csiaddonsv1alpha1.FenceState(u.object.Spec.ClusterFence) {
		return true, fmt.Errorf("fence state in the NetworkFence resource is not changed to %v yet",
			u.object.Spec.ClusterFence)
	}

	if nf.Status.Result != csiaddonsv1alpha1.FencingOperationResultSucceeded {
		setDRClusterUnfencingFailedCondition(&u.object.Status.Conditions, u.object.Generation,
			"unfencing operation not successful")

		u.log.Info("Unfencing operation not successful", "cluster", u.object.Name)

		return true, fmt.Errorf("un operation result not successful")
	}

	setDRClusterUnfencedCondition(&u.object.Status.Conditions, u.object.Generation,
		"Cluster successfully unfenced")
	u.advanceToNextPhase()

	return false, nil
}

func (u *drclusterInstance) requeueIfNFMWExists(peerCluster *ramen.DRCluster) (bool, error) {
	_, mwErr := u.mwUtil.FindManifestWorkByType(util.MWTypeNF, peerCluster.Name)
	if mwErr != nil {
		if k8serrors.IsNotFound(mwErr) {
			u.log.Info("NetworkFence and MW for it not found. Cleaned")

			return false, nil
		}

		return true, fmt.Errorf("failed to get MW for NetworkFence %w", mwErr)
	}

	return true, fmt.Errorf("NetworkFence not found. But MW still exists")
}

// We are here means following things have been confirmed.
// 1) Fencing CR MCV was obtained.
// 2) MCV for the Fencing CR showed the cluster as unfenced
//
// * Proceed to delete the ManifestWork for the fencingCR
// * Issue a requeue
func (u *drclusterInstance) cleanClusters(clusters []ramen.DRCluster) (bool, error) {
	u.log.Info("initiating the removal of NetworkFence resource ")

	needRequeue := false
	cleanedCount := 0

	for _, cluster := range clusters {
		// Can just error alone be checked?
		requeue, err := u.removeFencingCR(cluster)
		if err != nil {
			needRequeue = true
		} else {
			if !requeue {
				cleanedCount++
			} else {
				needRequeue = true
			}
		}
	}

	switch cleanedCount {
	case len(clusters):
		setDRClusterCleanCondition(&u.object.Status.Conditions, u.object.Generation, "fencing resource cleaned from cluster")
	default:
		setDRClusterCleaningCondition(&u.object.Status.Conditions, u.object.Generation, "NetworkFence resource clean started")
	}

	return needRequeue, nil
}

func (u *drclusterInstance) removeFencingCR(cluster ramen.DRCluster) (bool, error) {
	u.log.Info(fmt.Sprintf("cleaning the cluster fence resource from the cluster %s", cluster.Name))

	err := u.mwUtil.DeleteManifestWork(fmt.Sprintf(util.ManifestWorkNameFormat,
		u.object.Name, cluster.Name, util.MWTypeNF), cluster.Name)
	if err != nil {
		return true, fmt.Errorf("failed to delete NetworkFence resource from cluster %s", cluster.Name)
	}

	return false, nil
}

func getPeerCluster(ctx context.Context, list ramen.DRPolicyList, reconciler *DRClusterReconciler,
	object *ramen.DRCluster, log logr.Logger,
) (ramen.DRCluster, error) {
	var peerCluster ramen.DRCluster

	found := false

	log.Info(fmt.Sprintf("number of DRPolicies found: %d", len(list.Items)))

	for i := range list.Items {
		drp := &list.Items[i]

		log.Info(fmt.Sprintf("DRPolicy: %s, DRClusters: (%d) %v", drp.Name, len(drp.Spec.DRClusters),
			drp.Spec.DRClusters))

		// TODO: let policy = [e1, e2, e3]. Now, if e1 has to be fenced off,
		//       it will be created on either of e2 or e3. And later when e1
		//       has to be unfenced, the unfence should go to the same cluster
		//       where fencing CR was created. For now, assumption is that
		//       drPolicies will be having 2 clusters.
		for _, cluster := range drp.Spec.DRClusters {
			// skip if cluster is this drCluster
			if cluster == object.Name {
				drCluster, err := getPeerFromPolicy(ctx, reconciler, log, drp, object)
				if err != nil {
					log.Error(err, fmt.Sprintf("failed to get peer cluster for cluster %s", cluster))

					break
				}

				peerCluster = *drCluster
				found = true

				break
			}
		}

		if found {
			break
		}
	}

	if !found {
		return peerCluster, fmt.Errorf("failed to find the peer cluster for cluster %s", object.Name)
	}

	return peerCluster, nil
}

func getPeerFromPolicy(ctx context.Context, reconciler *DRClusterReconciler, log logr.Logger,
	drPolicy *ramen.DRPolicy, drCluster *ramen.DRCluster,
) (*ramen.DRCluster, error) {
	peerCluster := &ramen.DRCluster{}
	found := false

	for _, cluster := range drPolicy.Spec.DRClusters {
		if cluster == drCluster.Name {
			// skip myself
			continue
		}

		// search for the drCluster object for the peer cluster in the
		// same namespace as this cluster
		if err := reconciler.APIReader.Get(ctx,
			types.NamespacedName{Name: cluster, Namespace: drCluster.Namespace}, peerCluster); err != nil {
			log.Error(err, fmt.Sprintf("failed to get the DRCluster resource with name %s", cluster))
			// for now continue. As we just need to get one DRCluster with matching
			// region.
			continue
		}

		if util.ResourceIsDeleted(peerCluster) {
			log.Info(fmt.Sprintf("peer cluster %s of cluster %s is being deleted",
				peerCluster.Name, drCluster.Name))
			// for now continue. We just need to get one DRCluster with
			// matching region
			continue
		}

		if len(drPolicy.Status.Sync.PeerClasses) > 0 {
			found = true

			break
		}

		if drCluster.Spec.Region == peerCluster.Spec.Region {
			found = true

			break
		}
	}

	if !found {
		return nil, fmt.Errorf("count not find the peer cluster for %s", drCluster.Name)
	}

	return peerCluster, nil
}

func setDRClusterInitialCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusConditionIfNotFound(conditions, metav1.Condition{
		Type:               ramen.DRClusterValidated,
		Reason:             DRClusterConditionReasonInitializing,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionUnknown,
		Message:            message,
	})
	util.SetStatusConditionIfNotFound(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonInitializing,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionUnknown,
		Message:            message,
	})
	util.SetStatusConditionIfNotFound(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonInitializing,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionUnknown,
		Message:            message,
	})
}

// sets conditions when DRCluster is being fenced
// This means, a ManifestWork has been just created
// for NetworkFence CR and we have not yet seen the
// status of it.
// unfence = true, fence = false, clean = true
func setDRClusterFencingCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonFencing,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonFencing,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionTrue,
		Message:            message,
	})
}

// sets conditions when DRCluster is being unfenced
// This means, a ManifestWork has been just created/updated
// for NetworkFence CR and we have not yet seen the
// status of it.
// clean is false, because, the cluster is already fenced
// due to NetworkFence CR.
// unfence = false, fence = true, clean = false
func setDRClusterUnfencingCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonUnfencing,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionTrue,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonUnfencing,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
}

// sets conditions when the NetworkFence CR is being cleaned
// This means, a ManifestWork has been just deleted for
// NetworkFence CR and we have not yet seen the
// status of it.
// clean is false, because, it is yet not sure if the NetworkFence
// CR has been deleted or not.
// unfence = true, fence = false, clean = false
// TODO: Remove the linter skip when this function is used
func setDRClusterCleaningCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonCleaning,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonCleaning,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
}

// DRCluster is validated
func setDRClusterValidatedCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterValidated,
		Reason:             DRClusterConditionReasonValidated,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionTrue,
		Message:            message,
	})
}

// sets conditions when cluster has been successfully
// fenced via NetworkFence CR which still exists.
// Hence clean is false.
// unfence = false, fence = true, clean = false
func setDRClusterFencedCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonFenced,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionTrue,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonFenced,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
}

// sets conditions when cluster has been successfully
// unfenced via NetworkFence CR which still exists.
// Hence clean is false.
// unfence = true, fence = false, clean = false
func setDRClusterUnfencedCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonUnfenced,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonFenced,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
}

// sets conditions when the NetworkFence CR for this cluster
// has been successfully deleted. Since cleaning of NetworkFence
// CR is done after a successful unfence,
// unfence = true, fence = false, clean = true
func setDRClusterCleanCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonClean,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonClean,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionTrue,
		Message:            message,
	})
}

// sets conditions when the attempt to fence the cluster
// fails. Since, fencing is done via NetworkFence CR, after
// on a clean machine assumed to have no fencing CRs for
// this cluster,
// unfence = true, fence = false, clean = true
// TODO: Remove the linter skip when this function is used
func setDRClusterFencingFailedCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonFenceError,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonFenceError,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionTrue,
		Message:            message,
	})
}

// sets conditions when the attempt to unfence the cluster
// fails. Since, unfencing is done via NetworkFence CR, after
// successful fencing operation,
// unfence = false, fence = true, clean = false
// TODO: Remove the linter skip when this function is used
func setDRClusterUnfencingFailedCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonUnfenceError,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionTrue,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonUnfenceError,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
}

// sets conditions when the attempt to delete the fencing CR
// fails. Since, cleaning is always called after a successful
// Unfence operation, unfence = true, fence = false, clean = false
// TODO: Remove the linter skip when this function is used
//
//nolint:unused
func setDRClusterCleaningFailedCondition(conditions *[]metav1.Condition, observedGeneration int64, message string) {
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeFenced,
		Reason:             DRClusterConditionReasonCleanError,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
	util.SetStatusCondition(conditions, metav1.Condition{
		Type:               ramen.DRClusterConditionTypeClean,
		Reason:             DRClusterConditionReasonCleanError,
		ObservedGeneration: observedGeneration,
		Status:             metav1.ConditionFalse,
		Message:            message,
	})
}

func (u *drclusterInstance) createNFManifestWork(targetCluster *ramen.DRCluster, peerCluster *ramen.DRCluster,
	log logr.Logger, networkFenceClassName string,
) error {
	// create NetworkFence ManifestWork
	log.Info(fmt.Sprintf("Creating NetworkFence ManifestWork on cluster %s to perform fencing op on cluster %s",
		peerCluster.Name, targetCluster.Name))

	nf, err := generateNF(targetCluster, networkFenceClassName)
	if err != nil {
		return fmt.Errorf("failed to generate network fence resource: %w", err)
	}

	annotations := make(map[string]string)
	annotations[DRClusterNameAnnotation] = u.object.Name

	if err := u.mwUtil.CreateOrUpdateNFManifestWork(
		u.object.Name,
		peerCluster.Name, nf, annotations); err != nil {
		log.Error(err, "failed to create or update NetworkFence manifest")

		return fmt.Errorf("failed to create or update NetworkFence manifest in cluster %s to fence off cluster %s (%w)",
			peerCluster.Name, targetCluster.Name, err)
	}

	return nil
}

// this function fills the storage specific details in the NetworkFence resource.
// Currently it fills those details based on the annotations that are set on the
// DRCluster resource. However, in future it can be changed to get the storage
// specific details (such as driver, parameters, secret etc) from the status of
// the DRCluster resource.
func fillStorageDetails(cluster *ramen.DRCluster, nf *csiaddonsv1alpha1.NetworkFence) error {
	storageDriver, ok := cluster.Annotations[StorageAnnotationDriver]
	if !ok {
		return fmt.Errorf("failed to find storage driver in annotations")
	}

	storageSecretName, ok := cluster.Annotations[StorageAnnotationSecretName]
	if !ok {
		return fmt.Errorf("failed to find storage secret name in annotations")
	}

	storageSecretNamespace, ok := cluster.Annotations[StorageAnnotationSecretNamespace]
	if !ok {
		return fmt.Errorf("failed to find storage secret namespace in annotations")
	}

	clusterID, ok := cluster.Annotations[StorageAnnotationClusterID]
	if !ok {
		return fmt.Errorf("failed to find storage cluster id in annotations")
	}

	parameters := map[string]string{"clusterID": clusterID}

	nf.Spec.Secret.Name = storageSecretName
	nf.Spec.Secret.Namespace = storageSecretNamespace
	nf.Spec.Driver = storageDriver
	nf.Spec.Parameters = parameters

	return nil
}

// generateNF creates a NetworkFence resource for the target cluster. When a NetworkFenceClassName
// is provided, it's included in the resource; otherwise, it falls back to filling storage details directly.
// The resource includes CIDRs and fence state from the DRCluster specification.
// Resource naming pattern:
//   - Without NetworkFenceClass: "network-fence-" + cluster name
//   - With NetworkFenceClass: "network-fence-" + NFClass name + "-" + cluster name
func generateNF(targetCluster *ramen.DRCluster, networkFenceClassName string) (csiaddonsv1alpha1.NetworkFence, error) {
	if len(targetCluster.Spec.CIDRs) == 0 {
		return csiaddonsv1alpha1.NetworkFence{}, fmt.Errorf("CIDRs has no values")
	}

	resourceName := strings.Join([]string{NetworkFencePrefix, targetCluster.Name}, "-")

	nf := csiaddonsv1alpha1.NetworkFence{
		TypeMeta:   metav1.TypeMeta{Kind: "NetworkFence", APIVersion: "csiaddons.openshift.io/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: resourceName},
		Spec: csiaddonsv1alpha1.NetworkFenceSpec{
			FenceState: csiaddonsv1alpha1.FenceState(targetCluster.Spec.ClusterFence),
			Cidrs:      targetCluster.Spec.CIDRs,
		},
	}
	util.AddLabel(&nf, util.CreatedByRamenLabel, "true")

	if networkFenceClassName != "" {
		nf.Name = strings.Join([]string{NetworkFencePrefix, networkFenceClassName, targetCluster.Name}, "-")
		nf.Spec.NetworkFenceClassName = networkFenceClassName

		return nf, nil
	}

	if err := fillStorageDetails(targetCluster, &nf); err != nil {
		return nf, fmt.Errorf("failed to create network fence resource with storage detai: %w", err)
	}

	return nf, nil
}

//nolint:exhaustive
func (u *drclusterInstance) isFencingOrFenced() bool {
	switch u.getLastDRClusterPhase() {
	case ramen.Fencing:
		fallthrough
	case ramen.Fenced:
		return true
	}

	return false
}

//nolint:exhaustive
func (u *drclusterInstance) isUnfencingOrUnfenced() bool {
	switch u.getLastDRClusterPhase() {
	case ramen.Unfencing:
		fallthrough
	case ramen.Unfenced:
		return true
	}

	return false
}

func (u *drclusterInstance) getLastDRClusterPhase() ramen.DRClusterPhase {
	return u.object.Status.Phase
}

func (u *drclusterInstance) setDRClusterPhase(nextPhase ramen.DRClusterPhase) {
	if u.object.Status.Phase != nextPhase {
		u.log.Info(fmt.Sprintf("Phase: Current '%s'. Next '%s'",
			u.object.Status.Phase, nextPhase))

		u.object.Status.Phase = nextPhase
	}
}

func (u *drclusterInstance) advanceToNextPhase() {
	lastPhase := u.getLastDRClusterPhase()
	nextPhase := lastPhase

	switch lastPhase {
	case ramen.Fencing:
		nextPhase = ramen.Fenced
	case ramen.Unfencing:
		nextPhase = ramen.Unfenced
	case ramen.Available:
	case ramen.Fenced:
	case ramen.Unfenced:
	case ramen.Starting:
	}

	u.setDRClusterPhase(nextPhase)
}
