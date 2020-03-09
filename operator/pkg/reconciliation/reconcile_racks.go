package reconciliation

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/riptano/dse-operator/operator/pkg/apis/cassandra/v1alpha2"
	"github.com/riptano/dse-operator/operator/pkg/oplabels"
	"github.com/riptano/dse-operator/operator/pkg/utils"
)

// TODO remove this interface
// Reconciler ...
type Reconciler interface {
	Apply() (reconcile.Result, error)
}

// ReconcileRacks ...
type ReconcileRacks struct {
	ReconcileContext       *ReconciliationContext
	desiredRackInformation []*RackInformation
	statefulSets           []*appsv1.StatefulSet
}

var (
	ResultShouldNotRequeue     reconcile.Result = reconcile.Result{Requeue: false}
	ResultShouldRequeueNow     reconcile.Result = reconcile.Result{Requeue: true}
	ResultShouldRequeueSoon    reconcile.Result = reconcile.Result{Requeue: true, RequeueAfter: 2 * time.Second}
	ResultShouldRequeueTenSecs reconcile.Result = reconcile.Result{Requeue: true, RequeueAfter: 10 * time.Second}
)

// CalculateRackInformation determine how many nodes per rack are needed
func (r *ReconcileRacks) CalculateRackInformation() (Reconciler, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::calculateRackInformation")

	// Create RackInformation

	nodeCount := int(r.ReconcileContext.Datacenter.Spec.Size)
	racks := r.ReconcileContext.Datacenter.Spec.GetRacks()
	rackCount := len(racks)

	// TODO error if nodeCount < rackCount

	if r.ReconcileContext.Datacenter.Spec.Parked {
		nodeCount = 0
	}

	// 3 seeds per datacenter (this could be two, but we would like three seeds per cluster
	// and it's not easy for us to know if we're in a multi DC cluster in this part of the code)
	// OR all of the nodes, if there's less than 3
	// OR one per rack if there are four or more racks
	seedCount := 3
	if nodeCount < 3 {
		seedCount = nodeCount
	} else if rackCount > 3 {
		seedCount = rackCount
	}

	var desiredRackInformation []*RackInformation

	if rackCount < 1 {
		return nil, fmt.Errorf("assertion failed! rackCount should not possibly be zero here")
	}

	// nodes_per_rack = total_size / rack_count + 1 if rack_index < remainder

	nodesPerRack, extraNodes := nodeCount/rackCount, nodeCount%rackCount
	seedsPerRack, extraSeeds := seedCount/rackCount, seedCount%rackCount

	for rackIndex, currentRack := range racks {
		nodesForThisRack := nodesPerRack
		if rackIndex < extraNodes {
			nodesForThisRack++
		}
		seedsForThisRack := seedsPerRack
		if rackIndex < extraSeeds {
			seedsForThisRack++
		}
		nextRack := &RackInformation{}
		nextRack.RackName = currentRack.Name
		nextRack.NodeCount = nodesForThisRack
		nextRack.SeedCount = seedsForThisRack

		desiredRackInformation = append(desiredRackInformation, nextRack)
	}

	statefulSets := make([]*appsv1.StatefulSet, len(desiredRackInformation), len(desiredRackInformation))

	return &ReconcileRacks{
		ReconcileContext:       r.ReconcileContext,
		desiredRackInformation: desiredRackInformation,
		statefulSets:           statefulSets,
	}, nil
}

func (r *ReconcileRacks) CheckRackCreation() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackCreation")

	for idx := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]

		// Does this rack have a statefulset?

		statefulSet, statefulSetFound, err := r.GetStatefulSetForRack(rackInfo)
		if err != nil {
			r.ReconcileContext.ReqLogger.Error(
				err,
				"Could not locate statefulSet for",
				"Rack", rackInfo.RackName)
			res := &ResultShouldNotRequeue
			return res, err
		}

		if statefulSetFound == false {
			r.ReconcileContext.ReqLogger.Info(
				"Need to create new StatefulSet for",
				"Rack", rackInfo.RackName)
			res, err := r.ReconcileNextRack(statefulSet)
			if err != nil {
				r.ReconcileContext.ReqLogger.Error(
					err,
					"error creating new StatefulSet",
					"Rack", rackInfo.RackName)
				return &res, err
			}
		}

		r.statefulSets[idx] = statefulSet
	}

	return nil, nil
}

func (r *ReconcileRacks) CheckRackPodTemplate() (*reconcile.Result, error) {
	logger := r.ReconcileContext.ReqLogger
	logger.Info("starting CheckRackPodTemplate()")

	for idx := range r.desiredRackInformation {
		rackName := r.desiredRackInformation[idx].RackName
		if r.ReconcileContext.Datacenter.Spec.CanaryUpgrade && idx > 0 {
			logger.
				WithValues("rackName", rackName).
				Info("Skipping rack because CanaryUpgrade is turned on")
			return nil, nil
		}
		statefulSet := r.statefulSets[idx]

		// have to use zero here, because each statefulset is created with no replicas
		// in GetStatefulSetForRack()
		desiredSts, err := newStatefulSetForCassandraDatacenter(rackName, r.ReconcileContext.Datacenter, 0)
		if err != nil {
			logger.Error(err, "error calling newStatefulSetForCassandraDatacenter")
			res := ResultShouldNotRequeue
			return &res, err
		}

		needsUpdate := false

		currentHash := statefulSet.Annotations[resourceHashAnnotationKey]
		desiredHash := desiredSts.Annotations[resourceHashAnnotationKey]
		if currentHash != desiredHash {
			logger.
				WithValues("rackName", rackName).
				Info("statefulset needs an update")

			needsUpdate = true

			// "fix" the replica count, and maintain labels and annotations the k8s admin may have set
			desiredSts.Spec.Replicas = statefulSet.Spec.Replicas
			desiredSts.Labels = utils.MergeMap(map[string]string{}, statefulSet.Labels, desiredSts.Labels)
			desiredSts.Annotations = utils.MergeMap(map[string]string{}, statefulSet.Annotations, desiredSts.Annotations)

			desiredSts.DeepCopyInto(statefulSet)
		}

		if needsUpdate {

			if err := addOperatorProgressLabel(r.ReconcileContext, updating); err != nil {
				return &ResultShouldNotRequeue, err
			}

			logger.Info("Updating statefulset pod specs",
				"statefulSet", statefulSet,
			)

			err = r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, statefulSet)
			if err != nil {
				logger.Error(
					err,
					"Unable to perform update on statefulset for config",
					"statefulSet", statefulSet)
				res := ResultShouldNotRequeue
				return &res, err
			}

			// we just updated k8s and pods will be knocked out of ready state, so let k8s
			// call us back when these changes are done and the new pods are back to ready
			res := ResultShouldNotRequeue
			return &res, err
		} else {

			// the pod template is right, but if any pods don't match it,
			// or are missing, we should not move onto the next rack,
			// because there's an upgrade in progress

			status := statefulSet.Status
			if status.Replicas != status.ReadyReplicas ||
				status.Replicas != status.CurrentReplicas ||
				status.Replicas != status.UpdatedReplicas {

				logger.Info(
					"waiting for upgrade to finish on statefulset",
					"statefulset", statefulSet.Name,
					"replicas", status.Replicas,
					"readyReplicas", status.ReadyReplicas,
					"currentReplicas", status.CurrentReplicas,
					"updatedReplicas", status.UpdatedReplicas,
				)

				res := ResultShouldRequeueTenSecs
				return &res, nil
			}
		}
	}

	logger.Info("done CheckRackPodTemplate()")
	return nil, nil
}

func (r *ReconcileRacks) CheckRackLabels() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackLabels")

	for idx, _ := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]

		// Has this statefulset been reconciled?

		stsLabels := statefulSet.GetLabels()
		shouldUpdateLabels, updatedLabels := shouldUpdateLabelsForRackResource(stsLabels, r.ReconcileContext.Datacenter, rackInfo.RackName)

		if shouldUpdateLabels {
			r.ReconcileContext.ReqLogger.Info("Updating labels",
				"statefulSet", statefulSet,
				"current", stsLabels,
				"desired", updatedLabels)
			statefulSet.SetLabels(updatedLabels)

			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, statefulSet); err != nil {
				r.ReconcileContext.ReqLogger.Info("Unable to update statefulSet with labels",
					"statefulSet", statefulSet)
			}
		}
	}

	return nil, nil
}

func (r *ReconcileRacks) CheckRackParkedState() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackParkedState")

	for idx := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]

		parked := r.ReconcileContext.Datacenter.Spec.Parked
		currentPodCount := *statefulSet.Spec.Replicas

		var desiredNodeCount int32
		if parked {
			// rackInfo.NodeCount should be passed in as zero for parked clusters
			desiredNodeCount = int32(rackInfo.NodeCount)
		} else if currentPodCount > 1 {
			// already gone through the first round of scaling seed nodes, now lets add the rest of the nodes
			desiredNodeCount = int32(rackInfo.NodeCount)
		} else {
			// not parked and we just want to get our first seed up fully
			desiredNodeCount = int32(1)
		}

		if parked && currentPodCount > 0 {
			r.ReconcileContext.ReqLogger.Info(
				"CassandraDatacenter is parked, setting rack to zero replicas",
				"Rack", rackInfo.RackName,
				"currentSize", currentPodCount,
				"desiredSize", desiredNodeCount,
			)

			// TODO we should call a more graceful stop node command here

			res, err := r.UpdateRackNodeCount(statefulSet, desiredNodeCount)
			if err != nil {
				return &res, err
			}
		}
	}

	return nil, nil
}

// checkSeedLabels loops over all racks and makes sure that the proper pods are labelled as seeds.
func (r *ReconcileRacks) checkSeedLabels() (int, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckSeedLabels")
	seedCount := 0
	for idx := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		n, err := r.labelSeedPods(rackInfo)
		seedCount += n
		if err != nil {
			return 0, err
		}
	}
	return seedCount, nil
}

// CheckPodsReady loops over all the server pods and starts them
func (r *ReconcileRacks) CheckPodsReady() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckPodsReady")

	if r.ReconcileContext.Datacenter.Spec.Parked {
		return nil, nil
	}

	// all errors in this function we're going to treat as likely ephemeral problems that would resolve
	// so we use ResultShouldRequeueSoon to check again soon

	// successes where we want to end this reconcile loop, we generally also want to wait a bit
	// because stuff is happening concurrently in k8s (getting pods from pending to running)
	// or dse (getting a node bootstrapped and ready), so we use ResultShouldRequeueSoon to try again soon

	podList, err := listPods(r.ReconcileContext, r.ReconcileContext.Datacenter.GetDatacenterLabels())
	if err != nil {
		return &ResultShouldRequeueSoon, err
	}

	// step 0 - get the nodes labelled as seeds before we start any nodes

	seedCount, err := r.checkSeedLabels()
	if err != nil {
		return &ResultShouldRequeueSoon, err
	}
	err = refreshSeeds(r.ReconcileContext)
	if err != nil {
		return &ResultShouldRequeueSoon, err
	}

	// step 1 - see if any nodes are already coming up

	nodeIsStarting, err := r.findStartingNodes(podList)

	if err != nil || nodeIsStarting {
		return &ResultShouldRequeueSoon, err
	}

	// step 2 - get one node up per rack

	rackWaitingForANode, err := r.startOneNodePerRack(seedCount)

	if err != nil || rackWaitingForANode != "" {
		return &ResultShouldRequeueSoon, err
	}

	// step 3 - see if any nodes lost their readiness
	// or gained it back

	nodeStartedNotReady, err := r.findStartedNotReadyNodes(podList)

	if err != nil || nodeStartedNotReady {
		return &ResultShouldRequeueSoon, err
	}

	// step 4 - get all nodes up

	// if the cluster isn't healthy, that's ok, but go back to step 1
	if !isClusterHealthy(r.ReconcileContext) {
		r.ReconcileContext.ReqLogger.Info(
			"cluster isn't healthy",
		)
		return &ResultShouldRequeueSoon, nil
	}

	needsMoreNodes, err := r.startAllNodes(podList)
	if err != nil {
		return &ResultShouldRequeueSoon, err
	}
	if needsMoreNodes {
		return &ResultShouldRequeueSoon, nil
	}

	// step 4 sanity check that all pods are labelled as started and are ready

	readyPodCount, startedLabelCount := r.countReadyAndStarted(podList)
	desiredSize := int(r.ReconcileContext.Datacenter.Spec.Size)

	if desiredSize == readyPodCount && desiredSize == startedLabelCount {
		return nil, nil
	} else {
		err := fmt.Errorf("sanity checks failed desired:%d, ready:%d, started:%d", desiredSize, readyPodCount, startedLabelCount)
		return &ResultShouldNotRequeue, err
	}
}

// CheckRackScale loops over each statefulset and makes sure that it has the right
// amount of desired replicas. At this time we can only increase the amount of replicas.
func (r *ReconcileRacks) CheckRackScale() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackScale")

	for idx := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]

		// By the time we get here we know all the racks are ready for that particular size

		desiredNodeCount := int32(rackInfo.NodeCount)
		maxReplicas := *statefulSet.Spec.Replicas

		if maxReplicas < desiredNodeCount {

			// update it
			r.ReconcileContext.ReqLogger.Info(
				"Need to update the rack's node count",
				"Rack", rackInfo.RackName,
				"maxReplicas", maxReplicas,
				"desiredSize", desiredNodeCount,
			)

			res, err := r.UpdateRackNodeCount(statefulSet, desiredNodeCount)
			if err != nil {
				return &res, err
			}
		}

		currentReplicas := statefulSet.Status.CurrentReplicas
		if currentReplicas > desiredNodeCount {
			// too many ready replicas, how did this happen?
			r.ReconcileContext.ReqLogger.Info(
				"Too many replicas for StatefulSet",
				"desiredCount", desiredNodeCount,
				"currentCount", currentReplicas)
			res := ResultShouldNotRequeue
			return &res, fmt.Errorf("too many replicas")
		}
	}

	return nil, nil
}

// CheckRackPodLabels checks each pod and its volume(s) and makes sure they have the
// proper labels
func (r *ReconcileRacks) CheckRackPodLabels() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackPodLabels")

	for idx, _ := range r.desiredRackInformation {
		statefulSet := r.statefulSets[idx]

		if err := r.ReconcilePods(statefulSet); err != nil {
			res := ResultShouldNotRequeue
			return &res, nil
		}
	}

	return nil, nil
}

func shouldUpsertSuperUser(dc api.CassandraDatacenter) bool {
	lastCreated := dc.Status.SuperUserUpserted
	return time.Now().After(lastCreated.Add(time.Minute * 4))
}

func (r *ReconcileRacks) CreateSuperuser() (*reconcile.Result, error) {
	if r.ReconcileContext.Datacenter.Spec.Parked {
		r.ReconcileContext.ReqLogger.Info("cluster is parked, skipping CreateSuperuser")
		return nil, nil
	}

	rc := r.ReconcileContext

	//Skip upsert if already did so recently
	if !shouldUpsertSuperUser(*rc.Datacenter) {
		rc.ReqLogger.Info(fmt.Sprintf("The CQL superuser was last upserted at %v, skipping upsert", rc.Datacenter.Status.SuperUserUpserted))
		return nil, nil
	}

	rc.ReqLogger.Info("reconcile_racks::CreateSuperuser")

	// Get the secret

	if rc.Datacenter.Spec.SuperuserSecret == "" {
		rc.ReqLogger.Info("SuperuserSecret not specified for CassandraDatacenter.  Skipping superuser creation.")
		return nil, nil
	}

	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      rc.Datacenter.Spec.SuperuserSecret,
			Namespace: rc.Datacenter.Namespace,
		},
	}
	err := r.ReconcileContext.Client.Get(
		r.ReconcileContext.Ctx,
		types.NamespacedName{
			Name:      rc.Datacenter.Spec.SuperuserSecret,
			Namespace: rc.Datacenter.Namespace},
		secret)
	if err != nil {
		rc.ReqLogger.Error(err, "error retrieving SuperuserSecret for CassandraDatacenter.")
		return &ResultShouldNotRequeue, err
	}

	// We will call mgmt API on the first pod

	selector := map[string]string{
		api.ClusterLabel: rc.Datacenter.Spec.ClusterName,
	}
	podList, err := listPods(rc, selector)
	if err != nil {
		rc.ReqLogger.Error(err, "no pods found for CassandraDatacenter")
		return &ResultShouldNotRequeue, err
	}

	pod := podList.Items[0]

	err = rc.NodeMgmtClient.CallCreateRoleEndpoint(
		&pod,
		string(secret.Data["username"]),
		string(secret.Data["password"]))
	if err != nil {
		rc.ReqLogger.Error(err, "error creating superuser")
		return &ResultShouldNotRequeue, err
	}

	rc.Datacenter.Status.SuperUserUpserted = metav1.Now()
	if err = rc.Client.Status().Update(rc.Ctx, rc.Datacenter); err != nil {
		rc.ReqLogger.Error(err, "error updating the CQL superuser upsert timestamp")
		return &ResultShouldNotRequeue, err
	}

	return nil, nil
}

// Apply reconcileRacks determines if a rack needs to be reconciled.
func (r *ReconcileRacks) Apply() (reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::Apply")

	recResult, err := r.CheckRackCreation()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackLabels()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackParkedState()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackScale()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckPodsReady()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckDcPodDisruptionBudget()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackPodTemplate()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackPodLabels()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CreateSuperuser()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	if err := addOperatorProgressLabel(r.ReconcileContext, ready); err != nil {
		// this error is especially sad because we were just about to be done reconciling
		return ResultShouldRequeueNow, err
	}

	r.ReconcileContext.ReqLogger.Info("All StatefulSets should now be reconciled.")

	return reconcile.Result{}, nil
}

func isClusterHealthy(rc *ReconciliationContext) bool {
	selector := map[string]string{
		api.ClusterLabel: rc.Datacenter.Spec.ClusterName,
		// FIXME make a enum, pods should start in an Init state
		api.CassNodeState: "Started",
	}
	podList, err := listPods(rc, selector)
	if err != nil {
		rc.ReqLogger.Error(err, "no started pods found for CassandraDatacenter")
		return false
	}

	for _, pod := range podList.Items {
		err := rc.NodeMgmtClient.CallProbeClusterEndpoint(&pod, "LOCAL_QUORUM", len(rc.Datacenter.Spec.Racks))
		if err != nil {
			return false
		}
	}

	return true
}

// labelSeedPods iterates over all pods for a statefulset and makes sure the right number of
// ready pods are labelled as seeds, so that they are picked up by the headless seed service
// Returns the number of ready seeds.
func (r *ReconcileRacks) labelSeedPods(rackInfo *RackInformation) (int, error) {
	rackLabels := r.ReconcileContext.Datacenter.GetRackLabels(rackInfo.RackName)
	podList, err := listPods(r.ReconcileContext, rackLabels)
	if err != nil {
		r.ReconcileContext.ReqLogger.Error(
			err, "Unable to list pods for rack",
			"rackName", rackInfo.RackName)
		return 0, err
	}
	pods := podList.Items
	sort.SliceStable(pods, func(i, j int) bool {
		return pods[i].Name < pods[j].Name
	})
	count := 0
	for idx := range pods {
		pod := &pods[idx]
		podLabels := pod.GetLabels()
		ready := isServerReady(pod)
		starting := isServerStarting(pod)

		isSeed := ready && count < rackInfo.SeedCount
		currentVal := podLabels[api.SeedNodeLabel]
		if isSeed {
			count++
		}

		// this is the main place we label pods as seeds / not-seeds
		// the one exception to this is the very first node we bring up
		// in an empty cluster, and we set that node as a seed
		// in startOneNodePerRack()

		shouldUpdate := false
		if isSeed && currentVal != "true" {
			podLabels[api.SeedNodeLabel] = "true"
			shouldUpdate = true
		}
		// if this pod is starting, we should leave the seed label alone
		if !isSeed && currentVal == "true" && !starting {
			delete(podLabels, api.SeedNodeLabel)
			shouldUpdate = true
		}

		if shouldUpdate {
			pod.SetLabels(podLabels)
			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pod); err != nil {
				r.ReconcileContext.ReqLogger.Error(
					err, "Unable to update pod with seed label",
					"pod", pod.Name)
				return 0, err
			}
		}
	}
	return count, nil
}

// GetStatefulSetForRack returns the statefulset for the rack
// and whether it currently exists and whether an error occured
func (r *ReconcileRacks) GetStatefulSetForRack(
	nextRack *RackInformation) (*appsv1.StatefulSet, bool, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::getStatefulSetForRack")

	// Check if the desiredStatefulSet already exists
	currentStatefulSet := &appsv1.StatefulSet{}
	err := r.ReconcileContext.Client.Get(
		r.ReconcileContext.Ctx,
		newNamespacedNameForStatefulSet(r.ReconcileContext.Datacenter, nextRack.RackName),
		currentStatefulSet)

	if err == nil {
		return currentStatefulSet, true, nil
	}

	if !errors.IsNotFound(err) {
		return nil, false, err
	}

	desiredStatefulSet, err := newStatefulSetForCassandraDatacenter(
		nextRack.RackName,
		r.ReconcileContext.Datacenter,
		0)
	if err != nil {
		return nil, false, err
	}

	// Set the CassandraDatacenter as the owner and controller
	err = setControllerReference(
		r.ReconcileContext.Datacenter,
		desiredStatefulSet,
		r.ReconcileContext.Scheme)
	if err != nil {
		return nil, false, err
	}

	return desiredStatefulSet, false, nil
}

// ReconcileNextRack ensures that the resources for a rack have been properly created
// Note that each statefulset is using OrderedReadyPodManagement,
// so it will bring up one node at a time.
func (r *ReconcileRacks) ReconcileNextRack(statefulSet *appsv1.StatefulSet) (reconcile.Result, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::reconcileNextRack")

	if err := addOperatorProgressLabel(r.ReconcileContext, updating); err != nil {
		return ResultShouldNotRequeue, err
	}

	// Create the StatefulSet

	r.ReconcileContext.ReqLogger.Info(
		"Creating a new StatefulSet.",
		"statefulSetNamespace", statefulSet.Namespace,
		"statefulSetName", statefulSet.Name)
	if err := r.ReconcileContext.Client.Create(r.ReconcileContext.Ctx, statefulSet); err != nil {
		return ResultShouldNotRequeue, err
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileRacks) CheckDcPodDisruptionBudget() (*reconcile.Result, error) {
	// Create a PodDisruptionBudget for the CassandraDatacenter
	dseDc := r.ReconcileContext.Datacenter
	ctx := r.ReconcileContext.Ctx
	desiredBudget := newPodDisruptionBudgetForDatacenter(dseDc)

	// Set CassandraDatacenter as the owner and controller
	if err := setControllerReference(dseDc, desiredBudget, r.ReconcileContext.Scheme); err != nil {
		res := &ResultShouldRequeueNow
		return res, err
	}

	// Check if the budget already exists
	currentBudget := &policyv1beta1.PodDisruptionBudget{}
	err := r.ReconcileContext.Client.Get(
		ctx,
		types.NamespacedName{
			Name:      desiredBudget.Name,
			Namespace: desiredBudget.Namespace},
		currentBudget)

	// it's not possible to update a PodDisruptionBudget, so we need to delete this one and remake it
	if err == nil && currentBudget.Spec.MinAvailable.IntValue() != desiredBudget.Spec.MinAvailable.IntValue() {
		r.ReconcileContext.ReqLogger.Info(
			"Deleting and re-creating a PodDisruptionBudget",
			"pdbNamespace", desiredBudget.Namespace,
			"pdbName", desiredBudget.Name,
			"oldMinAvailable", currentBudget.Spec.MinAvailable,
			"desiredMinAvailable", desiredBudget.Spec.MinAvailable,
		)
		err = r.ReconcileContext.Client.Delete(ctx, currentBudget)
		if err == nil {
			err = r.ReconcileContext.Client.Create(ctx, desiredBudget)
		}
		// either way we return here
		res := &ResultShouldRequeueNow
		return res, err
	}

	if err != nil && errors.IsNotFound(err) {
		// Create the Budget
		r.ReconcileContext.ReqLogger.Info(
			"Creating a new PodDisruptionBudget.",
			"pdbNamespace", desiredBudget.Namespace,
			"pdbName", desiredBudget.Name)
		err = r.ReconcileContext.Client.Create(ctx, desiredBudget)
		res := &ResultShouldRequeueNow
		// either way we return here
		return res, err
	}

	if err != nil {
		res := &ResultShouldRequeueNow
		return res, err
	}

	return nil, nil
}

// UpdateRackNodeCount ...
func (r *ReconcileRacks) UpdateRackNodeCount(statefulSet *appsv1.StatefulSet, newNodeCount int32) (reconcile.Result, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::updateRack")

	r.ReconcileContext.ReqLogger.Info(
		"updating StatefulSet node count",
		"statefulSetNamespace", statefulSet.Namespace,
		"statefulSetName", statefulSet.Name,
		"newNodeCount", newNodeCount,
	)

	if err := addOperatorProgressLabel(r.ReconcileContext, updating); err != nil {
		return ResultShouldRequeueNow, err
	}

	statefulSet.Spec.Replicas = &newNodeCount

	err := r.ReconcileContext.Client.Update(
		r.ReconcileContext.Ctx,
		statefulSet)

	return ResultShouldRequeueNow, err
}

// ReconcilePods ...
func (r *ReconcileRacks) ReconcilePods(statefulSet *appsv1.StatefulSet) error {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::ReconcilePods")

	for i := int32(0); i < statefulSet.Status.Replicas; i++ {
		podName := fmt.Sprintf("%s-%v", statefulSet.Name, i)

		pod := &corev1.Pod{}
		err := r.ReconcileContext.Client.Get(
			r.ReconcileContext.Ctx,
			types.NamespacedName{
				Name:      podName,
				Namespace: statefulSet.Namespace},
			pod)
		if err != nil {
			r.ReconcileContext.ReqLogger.Error(
				err,
				"Unable to get pod",
				"Pod", podName,
			)
			return err
		}

		podLabels := pod.GetLabels()
		shouldUpdateLabels, updatedLabels := shouldUpdateLabelsForRackResource(podLabels, r.ReconcileContext.Datacenter, statefulSet.GetLabels()[api.RackLabel])
		if shouldUpdateLabels {
			r.ReconcileContext.ReqLogger.Info(
				"Updating labels",
				"Pod", podName,
				"current", podLabels,
				"desired", updatedLabels)
			pod.SetLabels(updatedLabels)

			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pod); err != nil {
				r.ReconcileContext.ReqLogger.Error(
					err,
					"Unable to update pod with label",
					"Pod", podName,
				)
			}
		}

		if pod.Spec.Volumes == nil || len(pod.Spec.Volumes) == 0 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
			continue
		}

		pvcName := pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName
		pvc := &corev1.PersistentVolumeClaim{
			TypeMeta: metav1.TypeMeta{
				Kind:       "PersistentVolumeClaim",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: statefulSet.Namespace,
			},
		}
		err = r.ReconcileContext.Client.Get(
			r.ReconcileContext.Ctx,
			types.NamespacedName{
				Name:      pvcName,
				Namespace: statefulSet.Namespace},
			pvc)
		if err != nil {
			r.ReconcileContext.ReqLogger.Error(
				err,
				"Unable to get pvc",
				"PVC", pvcName,
			)
			return err
		}

		pvcLabels := pvc.GetLabels()
		shouldUpdateLabels, updatedLabels = shouldUpdateLabelsForRackResource(pvcLabels, r.ReconcileContext.Datacenter, statefulSet.GetLabels()[api.RackLabel])
		if shouldUpdateLabels {
			r.ReconcileContext.ReqLogger.Info("Updating labels",
				"PVC", pvc,
				"current", pvcLabels,
				"desired", updatedLabels)

			pvc.SetLabels(updatedLabels)

			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pvc); err != nil {
				r.ReconcileContext.ReqLogger.Error(
					err,
					"Unable to update pvc with labels",
					"PVC", pvc,
				)
			}
		}
	}

	return nil
}

func mergeInLabelsIfDifferent(existingLabels, newLabels map[string]string) (bool, map[string]string) {
	updatedLabels := utils.MergeMap(map[string]string{}, existingLabels, newLabels)
	if reflect.DeepEqual(existingLabels, updatedLabels) {
		return false, existingLabels
	} else {
		return true, updatedLabels
	}
}

// shouldUpdateLabelsForClusterResource will compare the labels passed in with what the labels should be for a cluster level
// resource. It will return the updated map and a boolean denoting whether the resource needs to be updated with the new labels.
func shouldUpdateLabelsForClusterResource(resourceLabels map[string]string, dc *api.CassandraDatacenter) (bool, map[string]string) {
	desired := dc.GetClusterLabels()
	oplabels.AddManagedByLabel(desired)
	return mergeInLabelsIfDifferent(resourceLabels, desired)
}

// shouldUpdateLabelsForRackResource will compare the labels passed in with what the labels should be for a rack level
// resource. It will return the updated map and a boolean denoting whether the resource needs to be updated with the new labels.
func shouldUpdateLabelsForRackResource(resourceLabels map[string]string, dc *api.CassandraDatacenter, rackName string) (bool, map[string]string) {
	desired := dc.GetRackLabels(rackName)
	oplabels.AddManagedByLabel(desired)
	return mergeInLabelsIfDifferent(resourceLabels, desired)
}

// shouldUpdateLabelsForDatacenterResource will compare the labels passed in with what the labels should be for a datacenter level
// resource. It will return the updated map and a boolean denoting whether the resource needs to be updated with the new labels.
func shouldUpdateLabelsForDatacenterResource(resourceLabels map[string]string, dc *api.CassandraDatacenter) (bool, map[string]string) {
	desired := dc.GetDatacenterLabels()
	oplabels.AddManagedByLabel(desired)
	return mergeInLabelsIfDifferent(resourceLabels, desired)
}

func (r *ReconcileRacks) labelServerPodStarting(pod *corev1.Pod) error {
	client := r.ReconcileContext.Client
	ctx := r.ReconcileContext.Ctx
	dc := r.ReconcileContext.Datacenter
	pod.Labels[api.CassNodeState] = "Starting"
	err := client.Update(ctx, pod)
	if err != nil {
		return err
	}
	dc.Status.LastServerNodeStarted = metav1.Now()
	err = client.Status().Update(ctx, dc)
	return err
}

func (r *ReconcileRacks) labelServerPodStarted(pod *corev1.Pod) error {
	pod.Labels[api.CassNodeState] = "Started"
	err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pod)
	return err
}

func (r *ReconcileRacks) labelServerPodStartedNotReady(pod *corev1.Pod) error {
	pod.Labels[api.CassNodeState] = "Started-not-Ready"
	err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pod)
	return err
}

func (r *ReconcileRacks) callNodeManagementStart(pod *corev1.Pod) error {
	mgmtClient := r.ReconcileContext.NodeMgmtClient
	err := mgmtClient.CallLifecycleStartEndpoint(pod)

	return err
}

func (r *ReconcileRacks) findStartingNodes(podList *corev1.PodList) (bool, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::findStartingNodes")

	for idx := range podList.Items {
		pod := &podList.Items[idx]
		if pod.Labels[api.CassNodeState] == "Starting" {
			if isServerReady(pod) {
				if err := r.labelServerPodStarted(pod); err != nil {
					return false, err
				}
			} else {
				// TODO Calling start again on the pod seemed like a good defensive practice
				// TODO but was making problems w/ overloading management API
				// TODO Use a label to hold state and request starting no more than once per minute?

				// if err := r.callNodeManagementStart(pod); err != nil {
				// 	return false, err
				// }
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *ReconcileRacks) findStartedNotReadyNodes(podList *corev1.PodList) (bool, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::findStartedNotReadyNodes")

	for idx := range podList.Items {
		pod := &podList.Items[idx]
		if pod.Labels[api.CassNodeState] == "Started" {
			if !isServerReady(pod) {
				if err := r.labelServerPodStartedNotReady(pod); err != nil {
					return false, err
				}
				return true, nil
			}
		}
		if pod.Labels[api.CassNodeState] == "Started-not-Ready" {
			if isServerReady(pod) {
				if err := r.labelServerPodStarted(pod); err != nil {
					return false, err
				}
				return false, nil
			}
		}
	}
	return false, nil
}

// returns the name of one rack without any ready node
func (r *ReconcileRacks) startOneNodePerRack(readySeeds int) (string, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::startOneNodePerRack")

	selector := r.ReconcileContext.Datacenter.GetDatacenterLabels()
	podList, err := listPods(r.ReconcileContext, selector)
	if err != nil {
		return "", err
	}

	rackReadyCount := map[string]int{}
	for _, rackInfo := range r.desiredRackInformation {
		rackReadyCount[rackInfo.RackName] = 0
	}

	for idx := range podList.Items {
		pod := &podList.Items[idx]
		rackName := pod.Labels[api.RackLabel]
		if isServerReady(pod) {
			rackReadyCount[rackName]++
		}
	}

	// if the DC has no ready seeds, label a pod as a seed before we start DSE on it
	labelSeedBeforeStart := readySeeds == 0

	rackThatNeedsNode := ""
	for rackName, readyCount := range rackReadyCount {
		if readyCount > 0 {
			continue
		}
		rackThatNeedsNode = rackName
		for idx := range podList.Items {
			pod := &podList.Items[idx]
			mgmtApiUp := isMgmtApiRunning(pod)
			if !mgmtApiUp {
				continue
			}
			podRack := pod.Labels[api.RackLabel]
			if podRack == rackName {
				// this is the one exception to all seed labelling happening in labelSeedPods()
				if labelSeedBeforeStart {
					pod.Labels[api.SeedNodeLabel] = "true"
					if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pod); err != nil {
						return "", err
					}
					// sleeping five seconds for DNS paranoia
					time.Sleep(5 * time.Second)
				}
				if err = r.callNodeManagementStart(pod); err != nil {
					return "", err
				}
				if err = r.labelServerPodStarting(pod); err != nil {
					return "", err
				}
				return rackName, nil
			}
		}
	}

	return rackThatNeedsNode, nil
}

// returns whether one or more server nodes is not running or ready
func (r *ReconcileRacks) startAllNodes(podList *corev1.PodList) (bool, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::startAllNodes")

	for idx := range podList.Items {
		pod := &podList.Items[idx]
		if isMgmtApiRunning(pod) && !isServerReady(pod) && !isServerStarted(pod) {
			if err := r.callNodeManagementStart(pod); err != nil {
				return false, err
			}
			if err := r.labelServerPodStarting(pod); err != nil {
				return false, err
			}
			return true, nil
		}
	}

	// this extra pass only does anything when we have a combination of
	// ready server pods and pods that are not running - possibly stuck pending
	for idx := range podList.Items {
		pod := &podList.Items[idx]
		if !isMgmtApiRunning(pod) {
			r.ReconcileContext.ReqLogger.Info(
				"management api is not running on pod",
				"pod", pod.Name,
			)
			return true, nil
		}
	}

	return false, nil
}

func (r *ReconcileRacks) countReadyAndStarted(podList *corev1.PodList) (int, int) {
	ready := 0
	started := 0
	for idx := range podList.Items {
		pod := &podList.Items[idx]
		if isServerReady(pod) {
			ready++
			r.ReconcileContext.ReqLogger.Info(
				"found a ready pod",
				"podName", pod.Name,
				"runningCountReady", ready,
			)
		}
	}
	for idx := range podList.Items {
		pod := &podList.Items[idx]
		if isServerStarted(pod) {
			started++
			r.ReconcileContext.ReqLogger.Info(
				"found a pod we labeled Started",
				"podName", pod.Name,
				"runningCountStarted", started,
			)
		}
	}
	return ready, started
}

func isMgmtApiRunning(pod *corev1.Pod) bool {
	podStatus := pod.Status
	statuses := podStatus.ContainerStatuses
	for _, status := range statuses {
		if status.Name != "cassandra" {
			continue
		}
		state := status.State
		runInfo := state.Running
		if runInfo != nil {
			// give management API ten seconds to come up
			tenSecondsAgo := time.Now().Add(time.Second * -10)
			return runInfo.StartedAt.Time.Before(tenSecondsAgo)
		}
	}
	return false
}

func isServerStarting(pod *corev1.Pod) bool {
	return pod.Labels[api.CassNodeState] == "Starting"
}

func isServerStarted(pod *corev1.Pod) bool {
	return pod.Labels[api.CassNodeState] == "Started" ||
		pod.Labels[api.CassNodeState] == "Started-not-Ready"
}

func isServerReady(pod *corev1.Pod) bool {
	status := pod.Status
	statuses := status.ContainerStatuses
	for _, status := range statuses {
		if status.Name != "cassandra" {
			continue
		}
		return status.Ready
	}
	return false
}

func refreshSeeds(rc *ReconciliationContext) error {
	rc.ReqLogger.Info("reconcile_racks::refreshSeeds")
	if rc.Datacenter.Spec.Parked {
		rc.ReqLogger.Info("cluster is parked, skipping refreshSeeds")
		return nil
	}

	selector := rc.Datacenter.GetClusterLabels()
	selector[api.CassNodeState] = "Started"

	podList, err := listPods(rc, selector)
	if err != nil {
		rc.ReqLogger.Error(err, "error listing pods during refreshSeeds")
		return err
	}

	for _, pod := range podList.Items {
		if err := rc.NodeMgmtClient.CallReloadSeedsEndpoint(&pod); err != nil {
			return err
		}
	}

	return nil
}

func listPods(rc *ReconciliationContext, selector map[string]string) (*corev1.PodList, error) {
	rc.ReqLogger.Info("reconcile_racks::listPods")

	listOptions := &client.ListOptions{
		Namespace:     rc.Datacenter.Namespace,
		LabelSelector: labels.SelectorFromSet(selector),
	}

	podList := &corev1.PodList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
	}

	return podList, rc.Client.List(rc.Ctx, podList, listOptions)
}
