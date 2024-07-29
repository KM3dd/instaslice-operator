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
	"fmt"
	"regexp"
	"strings"
	"time"

	inferencev1alpha1 "codeflare.dev/instaslice/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// InstasliceReconciler reconciles a Instaslice object
type InstasliceReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	kubeClient *kubernetes.Clientset
}

// AllocationPolicy interface with a single method
type AllocationPolicy interface {
	SetAllocationDetails(profileName string, newStart, size uint32, podUUID string, nodename string, processed string, discoveredGiprofile int, Ciprofileid int, Ciengprofileid int, namespace string, podName string, gpuUuid string) *inferencev1alpha1.AllocationDetails
}

type RightToLeftPolicy struct{}

type LeftToRightPolicy struct{}

type FirstFitPolicy struct{}

// TODO: remove this and find a better way to reduce duplicates update via controller runtime
var processedPodDeletion []string

//+kubebuilder:rbac:groups=inference.codeflare.dev,resources=instaslices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=inference.codeflare.dev,resources=instaslices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=inference.codeflare.dev,resources=instaslices/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;create;update;patch;delete

func (r *InstasliceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	logger := log.Log.WithName("InstaSlice-controller")
	var policy AllocationPolicy
	policy = &FirstFitPolicy{}
	pod := &v1.Pod{}
	var isPodGated = false
	err := r.Get(ctx, req.NamespacedName, pod)
	if err != nil {
		if errors.IsNotFound(err) {
			// Pod not found. It might have been deleted.
			return ctrl.Result{}, nil
		}
		// Error fetching the Pod
		return ctrl.Result{}, err
	}

	isPodGated = checkIfPodGated(pod, isPodGated)

	var instasliceList inferencev1alpha1.InstasliceList

	if err := r.List(ctx, &instasliceList, &client.ListOptions{}); err != nil {
		logger.Error(err, "Error listing Instaslice")
	}
	// handles graceful termination of pods, wait for about 30 seconds from the time deletiontimestamp is set on the pod
	if !pod.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(pod, "org.instaslice/accelarator") && isPodDeletionProcessed(pod.Name, processedPodDeletion) {
			for _, instaslice := range instasliceList.Items {
				for podUuid, allocation := range instaslice.Spec.Allocations {
					if podUuid == string(pod.UID) {
						elapsed := time.Since(pod.DeletionTimestamp.Time)
						if elapsed > 30*time.Second {
							if controllerutil.RemoveFinalizer(pod, "org.instaslice/accelarator") {
								if err := r.Update(ctx, pod); err != nil {
									return ctrl.Result{}, err
								}
								logger.Info("finalizer deleted")
							}
							allocation.Allocationstatus = "deleted"
							instaslice.Spec.Allocations[podUuid] = allocation
							err := r.Update(ctx, &instaslice)
							if errors.IsConflict(err) {
								//not retrying as daemonset might be updating the instaslice object for other pods
								logger.Info("Latest version for instaslice object not found, retrying in next iteration")
								return ctrl.Result{Requeue: true}, nil
							}
							if err != nil {
								logger.Info("allocation set to deleted for", "pod", pod.Name)
								processedPodDeletion = append(processedPodDeletion, pod.Name)
							}
						} else {
							remainingTime := 30*time.Second - elapsed
							return ctrl.Result{RequeueAfter: remainingTime}, nil
						}
					}
				}

			}
		}
		//exit after handling deletion event for a pod.
		return ctrl.Result{}, nil
	}

	// find allocation in the cluster for the pod
	// set allocationstatus to creating when controller adds the allocation
	// check for allocationstatus as created when daemonset is done realizing the slice on the GPU node.
	// set allocationstatus to ungated and ungate the pod so that the workload can begin execution.
	if isPodGated {
		//Assume pod only has one container with one GPU requests
		if len(pod.Spec.Containers) != 1 {
			return ctrl.Result{}, fmt.Errorf("multiple containers per pod not supported")
		}
		limits := pod.Spec.Containers[0].Resources.Limits
		profileName := r.extractProfileName(limits)
		for _, instaslice := range instasliceList.Items {
			for podUuid, allocations := range instaslice.Spec.Allocations {
				if allocations.Allocationstatus == "created" && allocations.PodUUID == string(pod.UID) {
					pod := r.unGatePod(pod)
					errForUngating := r.Update(ctx, pod)
					if errors.IsConflict(errForUngating) {
						//pod updates are retried as controller is the only entiting working on pod updates.
						return ctrl.Result{Requeue: true}, nil
					}
					allocations.Allocationstatus = "ungated"
					instaslice.Spec.Allocations[podUuid] = allocations
				}

			}
			//pod does not have an allocation yet, make allocation
			if _, exists := instaslice.Spec.Allocations[string(pod.UID)]; !exists {
				r.findDeviceForASlice(&instaslice, profileName, policy, pod)
			}
			//update all created allocations belonging to different pods to state ungated
			if err := r.Update(ctx, &instaslice); err != nil {
				logger.Error(err, "Error updating instaslice allocations")
				return ctrl.Result{}, err
			}
		}

	}
	// no gated pod found, do nothing
	return ctrl.Result{}, nil
}

func (r *InstasliceReconciler) findDeviceForASlice(instaslice *inferencev1alpha1.Instaslice, profileName string, policy AllocationPolicy, pod *v1.Pod) (*inferencev1alpha1.Instaslice, error) {
	//TODO: discover this value, this may work for A100 and H100 for now.
	for gpuuuid, _ := range instaslice.Spec.MigGPUUUID {
		if instaslice.Spec.Allocations == nil {
			instaslice.Spec.Allocations = make(map[string]inferencev1alpha1.AllocationDetails)
		}
		newStart := r.getStartIndexFromPreparedState(instaslice, gpuuuid, profileName)
		//size cannot be 9 atleast for A100s 40GB/80GB and H100 variants
		notValidIndex := uint32(9)
		if newStart == notValidIndex {
			//Move to next GPU
			continue
		}
		size, discoveredGiprofile, Ciprofileid, Ciengprofileid := r.extractGpuProfile(instaslice, profileName)
		allocDetails := policy.SetAllocationDetails(profileName, uint32(newStart), uint32(size),
			string(pod.UID), instaslice.Name, "creating", discoveredGiprofile,
			Ciprofileid, Ciengprofileid, pod.Namespace, pod.Name, gpuuuid)
		instaslice.Spec.Allocations[string(pod.UID)] = *allocDetails
		return instaslice, nil
	}

	return instaslice, fmt.Errorf("failed to update instaslice object to state - creating")
}

// Extract profile name from the container limits spec
func (*InstasliceReconciler) extractProfileName(limits v1.ResourceList) string {
	profileName := ""
	for k, _ := range limits {
		if strings.Contains(k.String(), "nvidia") {

			re := regexp.MustCompile(`(\d+g\.\d+gb)`)
			match := re.FindStringSubmatch(k.String())
			if len(match) > 1 {
				profileName = match[1]
			} else {
				log.Log.Info("No match found")
			}
		}
	}
	return profileName
}

// Extract NVML specific attributes for GPUs, this will change for different generations of the GPU.
func (*InstasliceReconciler) extractGpuProfile(instaslice *inferencev1alpha1.Instaslice, profileName string) (int, int, int, int) {
	var size int
	var discoveredGiprofile int
	var Ciprofileid int
	var Ciengprofileid int
	for _, item := range instaslice.Spec.Migplacement {
		if item.Profile == profileName {
			for _, aPlacement := range item.Placements {
				size = aPlacement.Size
				discoveredGiprofile = item.Giprofileid
				Ciprofileid = item.CIProfileID
				Ciengprofileid = item.CIEngProfileID
				break
			}
		}
	}
	return size, discoveredGiprofile, Ciprofileid, Ciengprofileid
}

// accounting logic that finds the correct GPU and index where a slice could be placed.
func (*InstasliceReconciler) getStartIndexFromPreparedState(instaslice *inferencev1alpha1.Instaslice, gpuUUID string, profileName string) uint32 {
	//TODO: generalize, A100 and H100 have 8 indexes for 3g and 7g and 7 for rest, so go with 8 and we are bounded by
	//only valid placement indexes for a profile.
	var gpuAllocatedIndex [8]uint32
	// clean slate init
	for i := range gpuAllocatedIndex {
		gpuAllocatedIndex[i] = 0
	}
	for _, item := range instaslice.Spec.Prepared {
		if item.Parent == gpuUUID {
			for i := 0; i < int(item.Size); i++ {
				gpuAllocatedIndex[int(item.Start)+i] = 1
			}

		}
	}

	for _, item := range instaslice.Spec.Allocations {
		if item.GPUUUID == gpuUUID {
			for i := 0; i < int(item.Size); i++ {
				gpuAllocatedIndex[int(item.Start)+i] = 1
			}
		}
	}

	var neededContinousSlot int
	var possiblePlacements []int
	for _, placement := range instaslice.Spec.Migplacement {
		if placement.Profile == profileName {
			neededContinousSlot = placement.Placements[0].Size
			for _, placement := range placement.Placements {
				possiblePlacements = append(possiblePlacements, placement.Start)
			}
			break
		}
	}
	//TODO: generalize for other hardware models like A30, no slices can be placed on 9th index
	//if we return 9 then assume no valid index is found.
	var newStart = uint32(9)
	for _, value := range possiblePlacements {
		if gpuAllocatedIndex[value] == 0 {
			if neededContinousSlot == 1 {
				newStart = uint32(value)
				break
			}
			if neededContinousSlot == 2 {
				if value+neededContinousSlot < len(gpuAllocatedIndex) {
					if gpuAllocatedIndex[value] == 0 && gpuAllocatedIndex[value+1] == 0 {
						newStart = uint32(value)
						break
					}
				}

			}
			if neededContinousSlot == 4 {
				if value+neededContinousSlot < len(gpuAllocatedIndex) {
					if gpuAllocatedIndex[value] == 0 && gpuAllocatedIndex[value+1] == 0 && gpuAllocatedIndex[value+2] == 0 && gpuAllocatedIndex[value+3] == 0 {
						newStart = uint32(value)
						break
					}
				}
			}

			if neededContinousSlot == 8 {
				//special case
				if value+neededContinousSlot < len(gpuAllocatedIndex) {
					if gpuAllocatedIndex[value] == 0 && gpuAllocatedIndex[value+1] == 0 &&
						gpuAllocatedIndex[value+2] == 0 && gpuAllocatedIndex[value+3] == 0 &&
						gpuAllocatedIndex[value+4] == 0 && gpuAllocatedIndex[value+5] == 0 &&
						gpuAllocatedIndex[value+6] == 0 && gpuAllocatedIndex[value+7] == 0 {
						newStart = uint32(value)
					}
				}
			}
		}

	}

	return newStart
}

func checkIfPodGated(pod *v1.Pod, isPodGated bool) bool {
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == "org.instaslice/accelarator" {
			if pod.Status.Phase == v1.PodPending && strings.Contains(pod.Status.Conditions[0].Message, "blocked") {
				isPodGated = true
			}
		}
	}
	return isPodGated
}

// podMapFunc maps pods to instaslice created allocations
func (r *InstasliceReconciler) podMapFunc(ctx context.Context, obj client.Object) []reconcile.Request {
	instaslice := obj.(*inferencev1alpha1.Instaslice)
	for _, allocation := range instaslice.Spec.Allocations {
		if allocation.Allocationstatus == "created" || allocation.Allocationstatus == "deleting" {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: allocation.Namespace, Name: allocation.PodName}}}
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstasliceReconciler) SetupWithManager(mgr ctrl.Manager) error {

	restConfig := mgr.GetConfig()

	var err error
	r.kubeClient, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Pod{}).Named("InstaSlice-controller").
		Watches(&inferencev1alpha1.Instaslice{}, handler.EnqueueRequestsFromMapFunc(r.podMapFunc)).
		Complete(r)
}

func (r *InstasliceReconciler) unGatePod(podUpdate *v1.Pod) *v1.Pod {
	for i, gate := range podUpdate.Spec.SchedulingGates {
		if gate.Name == "org.instaslice/accelarator" {
			podUpdate.Spec.SchedulingGates = append(podUpdate.Spec.SchedulingGates[:i], podUpdate.Spec.SchedulingGates[i+1:]...)
		}
	}
	return podUpdate
}

// Policy based allocation - FirstFit
func (r *FirstFitPolicy) SetAllocationDetails(profileName string, newStart, size uint32, podUUID, nodename string,
	processed string, discoveredGiprofile int, Ciprofileid int, Ciengprofileid int,
	namespace string, podName string, gpuUuid string) *inferencev1alpha1.AllocationDetails {
	return &inferencev1alpha1.AllocationDetails{
		Profile:          profileName,
		Start:            uint32(newStart),
		Size:             uint32(size),
		PodUUID:          podUUID,
		Nodename:         nodename,
		Allocationstatus: processed,
		Giprofileid:      discoveredGiprofile,
		CIProfileID:      Ciprofileid,
		CIEngProfileID:   Ciengprofileid,
		Namespace:        namespace,
		PodName:          podName,
		GPUUUID:          gpuUuid,
	}
}

// Policy based allocation - LeftToRIght
func (l *LeftToRightPolicy) SetAllocationDetails(profileName string, newStart, size uint32, podUUID, nodename string,
	processed string, discoveredGiprofile int, Ciprofileid int, Ciengprofileid int,
	namespace string, podName string, gpuUuid string) *inferencev1alpha1.AllocationDetails {
	// Implement the left-to-right policy here
	return &inferencev1alpha1.AllocationDetails{}
}

// Policy based allocation - RigghToLeft
func (l *RightToLeftPolicy) SetAllocationDetails(profileName string, newStart, size uint32, podUUID, nodename string,
	processed string, discoveredGiprofile int, Ciprofileid int, Ciengprofileid int,
	namespace string, podName string, gpuUuid string) *inferencev1alpha1.AllocationDetails {
	// Implement the left-to-right policy here
	return &inferencev1alpha1.AllocationDetails{}
}

func isPodDeletionProcessed(str string, arr []string) bool {
	for _, v := range arr {
		if v == str {
			return false
		}
	}
	return true
}
