/*
Copyright 2023 The Kubernetes Authors.

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

package disruption_test

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha5"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Consolidation", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var node *v1.Node
	BeforeEach(func() {
		nodePool = test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Disruption: v1beta1.Disruption{
					ConsolidationPolicy: v1beta1.ConsolidationPolicyWhenUnderutilized,
				},
			},
		})
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
					v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID:  test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
			},
		})
	})
	Context("Empty", func() {
		var nodeClaim2 *v1beta1.NodeClaim
		var node2 *v1.Node

		BeforeEach(func() {
			nodeClaim2, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
		})
		It("can delete empty nodes", func() {
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the empty node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can delete multiple empty nodes", func() {
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)
			wg := sync.WaitGroup{}
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim, nodeClaim2)

			// we should delete the empty nodes
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			ExpectNotFound(ctx, env.Client, nodeClaim)
			ExpectNotFound(ctx, env.Client, nodeClaim2)
		})
		It("considers pending pods when consolidating", func() {
			largeTypes := lo.Filter(cloudProvider.InstanceTypes, func(item *cloudprovider.InstanceType, index int) bool {
				return item.Capacity.Cpu().Cmp(resource.MustParse("64")) >= 0
			})
			sort.Slice(largeTypes, func(i, j int) bool {
				return largeTypes[i].Offerings[0].Price < largeTypes[j].Offerings[0].Price
			})

			largeCheapType := largeTypes[0]
			nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   largeCheapType.Name,
						v1beta1.CapacityTypeLabelKey: largeCheapType.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         largeCheapType.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  *largeCheapType.Capacity.Cpu(),
						v1.ResourcePods: *largeCheapType.Capacity.Pods(),
					},
				},
			})

			// there is a pending pod that should land on the node
			pod := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU: resource.MustParse("1"),
					},
				},
			})
			unsched := test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU: resource.MustParse("62"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, nodeClaim, node, pod, unsched, nodePool)

			// bind one of the pods to the node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})

			// we don't need any new nodes and consolidation should notice the huge pending pod that needs the large
			// node to schedule, which prevents the large expensive node from being replaced
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
		})
	})
	Context("Replace", func() {
		It("can replace node", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			// consolidation won't delete the old nodeclaim until the new nodeclaim is ready
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// Expect that the new nodeclaim does not request the most expensive instance type
			Expect(nodeClaims[0].Name).ToNot(Equal(nodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirements(nodeClaims[0].Spec.Requirements...).Has(v1.LabelInstanceTypeStable)).To(BeTrue())
			Expect(scheduling.NewNodeSelectorRequirements(nodeClaims[0].Spec.Requirements...).Get(v1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can replace nodes if another nodePool returns no instance types", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			nodePool2 := test.NodePool()
			cloudProvider.InstanceTypesForNodePool[nodePool2.Name] = nil
			ExpectApplied(ctx, env.Client, rs, pod, node, nodeClaim, nodePool, nodePool2)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			// consolidation won't delete the old nodeclaim until the new nodeclaim is ready
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeclaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeclaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// Expect that the new nodeclaim does not request the most expensive instance type
			Expect(nodeclaims[0].Name).ToNot(Equal(nodeClaim.Name))
			Expect(scheduling.NewNodeSelectorRequirements(nodeclaims[0].Spec.Requirements...).Has(v1.LabelInstanceTypeStable)).To(BeTrue())
			Expect(scheduling.NewNodeSelectorRequirements(nodeclaims[0].Spec.Requirements...).Get(v1.LabelInstanceTypeStable).Has(mostExpensiveInstance.Name)).To(BeFalse())

			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can replace nodes, considers PDB", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         labels,
				MaxUnavailable: fromInt(0),
				Status: &policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 1,
					DisruptionsAllowed: 0,
					CurrentHealthy:     1,
					DesiredHealthy:     1,
					ExpectedPods:       1,
				},
			})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodePool, pdb)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})

			// we didn't create a new nodeclaim or delete the old one
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
			ExpectExists(ctx, env.Client, node)
		})
		It("can replace nodes, considers PDB policy", func() {
			if env.Version.Minor() < 27 {
				Skip("PDB policy ony enabled by default for K8s >= 1.27.x")
			}
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         labels,
				MaxUnavailable: fromInt(0),
				Status: &policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 1,
					DisruptionsAllowed: 0,
					CurrentHealthy:     1,
					DesiredHealthy:     1,
					ExpectedPods:       1,
				},
			})
			alwaysAllow := policyv1.AlwaysAllow
			pdb.Spec.UnhealthyPodEvictionPolicy = &alwaysAllow

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodePool, pdb)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node)

			// set all of these pods to unhealthy so the PDB won't stop their eviction
			for _, p := range pods {
				p.Status.Conditions = []v1.PodCondition{
					{
						Type:               v1.PodReady,
						Status:             v1.ConditionFalse,
						LastProbeTime:      metav1.Now(),
						LastTransitionTime: metav1.Now(),
					},
				}
				ExpectApplied(ctx, env.Client, p)
			}

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			// consolidation won't delete the old nodeclaim until the new nodeclaim is ready
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			nodeclaims := ExpectNodeClaims(ctx, env.Client)
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodeclaims).To(HaveLen(1))
			Expect(nodes).To(HaveLen(1))

			// we didn't create a new nodeclaim or delete the old one
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can replace nodes, PDB namespace must match", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			namespace := test.Namespace()
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace.ObjectMeta.Name,
				},
				Labels:         labels,
				MaxUnavailable: fromInt(0),
				Status: &policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 1,
					DisruptionsAllowed: 0,
					CurrentHealthy:     1,
					DesiredHealthy:     1,
					ExpectedPods:       1,
				},
			})

			// bind pods to node
			ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool, namespace, pdb)
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			// consolidation won't delete the old node until the new node is ready
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// should create a new nodeclaim as there is a cheaper one that can hold the pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can replace nodes, considers karpenter.sh/do-not-consolidate on nodes", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("2"),
					},
				},
			})
			annotatedNodeClaim, annotatedNode := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha5.DoNotConsolidateNodeAnnotationKey: "true",
					},
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("5"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, annotatedNodeClaim, annotatedNode)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], annotatedNode)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, annotatedNode}, []*v1beta1.NodeClaim{nodeClaim, annotatedNodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node and replace with a cheaper node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can replace nodes, considers karpenter.sh/do-not-disrupt on nodes", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("2"),
					},
				},
			})
			annotatedNodeClaim, annotatedNode := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1beta1.DoNotDisruptAnnotationKey: "true",
					},
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("5"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, annotatedNodeClaim, annotatedNode)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], annotatedNode)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, annotatedNode}, []*v1beta1.NodeClaim{nodeClaim, annotatedNodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node and replace with a cheaper node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can replace nodes, considers karpenter.sh/do-not-evict on pods", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("2"),
					},
				},
			})
			nodeClaim2, node2 := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("5"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			// Block this pod from being disrupted with karpenter.sh/do-not-evict
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node and replace with a cheaper node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can replace nodes, considers karpenter.sh/do-not-disrupt on pods", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("2"),
					},
				},
			})
			nodeClaim2, node2 := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("5"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			// Block this pod from being disrupted with karpenter.sh/do-not-disrupt
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1beta1.DoNotDisruptAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node and replace with a cheaper node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("won't replace node if any spot replacement is more expensive", func() {
			currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "current-on-demand",
				Offerings: []cloudprovider.Offering{
					{
						CapacityType: v1beta1.CapacityTypeOnDemand,
						Zone:         "test-zone-1a",
						Price:        0.5,
						Available:    false,
					},
				},
			})
			replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "potential-spot-replacement",
				Offerings: []cloudprovider.Offering{
					{
						CapacityType: v1beta1.CapacityTypeSpot,
						Zone:         "test-zone-1a",
						Price:        1.0,
						Available:    true,
					},
					{
						CapacityType: v1beta1.CapacityTypeSpot,
						Zone:         "test-zone-1b",
						Price:        0.2,
						Available:    true,
					},
					{
						CapacityType: v1beta1.CapacityTypeSpot,
						Zone:         "test-zone-1c",
						Price:        0.4,
						Available:    true,
					},
				},
			})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				currentInstance,
				replacementInstance,
			}

			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   currentInstance.Name,
						v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
			ExpectExists(ctx, env.Client, node)
		})
		It("won't replace on-demand node if on-demand replacement is more expensive", func() {
			currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "current-on-demand",
				Offerings: []cloudprovider.Offering{
					{
						CapacityType: v1beta1.CapacityTypeOnDemand,
						Zone:         "test-zone-1a",
						Price:        0.5,
						Available:    false,
					},
				},
			})
			replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "on-demand-replacement",
				Offerings: []cloudprovider.Offering{
					{
						CapacityType: v1beta1.CapacityTypeOnDemand,
						Zone:         "test-zone-1a",
						Price:        0.6,
						Available:    true,
					},
					{
						CapacityType: v1beta1.CapacityTypeOnDemand,
						Zone:         "test-zone-1b",
						Price:        0.6,
						Available:    true,
					},
					{
						CapacityType: v1beta1.CapacityTypeSpot,
						Zone:         "test-zone-1b",
						Price:        0.2,
						Available:    true,
					},
					{
						CapacityType: v1beta1.CapacityTypeSpot,
						Zone:         "test-zone-1c",
						Price:        0.3,
						Available:    true,
					},
				},
			})

			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				currentInstance,
				replacementInstance,
			}

			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			// nodePool should require on-demand instance for this test case
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
				{
					Key:      v1beta1.CapacityTypeLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1beta1.CapacityTypeOnDemand},
				},
			}
			nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   currentInstance.Name,
						v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         currentInstance.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")},
				},
			})

			ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Expect to not create or delete more nodeclaims
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim)
			ExpectExists(ctx, env.Client, node)
		})
	})
	Context("Delete", func() {
		var nodeClaim2 *v1beta1.NodeClaim
		var node2 *v1.Node

		BeforeEach(func() {
			nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaim2, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
		})
		It("can delete nodes", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		})
		It("can delete nodes if another nodePool has no node template", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			nodeClassNodePool := test.NodePool()
			nodeClassNodePool.Spec.Template.Spec.NodeClassRef = nil
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		})
		It("can delete nodes, when non-Karpenter capacity can fit pods", func() {
			labels := map[string]string{
				"app": "test",
			}
			unmanagedNode := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			})
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
			})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, unmanagedNode, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, unmanagedNode}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we can fit all of our pod capacity on the unmanaged node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can delete nodes, considers PDB", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			// only pod[2] is covered by the PDB
			pods[2].Labels = labels
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         labels,
				MaxUnavailable: fromInt(0),
				Status: &policyv1.PodDisruptionBudgetStatus{
					ObservedGeneration: 1,
					DisruptionsAllowed: 0,
					CurrentHealthy:     1,
					DesiredHealthy:     1,
					ExpectedPods:       1,
				},
			})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool, pdb)

			// two pods on node 1
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			// one on node 2, but it has a PDB with zero disruptions allowed
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we don't need a new node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// but we expect to delete the nodeclaim with more pods (node) as the pod on nodeClaim2 has a PDB preventing
			// eviction
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can delete nodes, considers karpneter.sh/do-not-consolidate on nodes", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			nodeClaim2.Annotations = lo.Assign(nodeClaim2.Annotations, map[string]string{v1alpha5.DoNotConsolidateNodeAnnotationKey: "true"})
			node2.Annotations = lo.Assign(nodeClaim2.Annotations, map[string]string{v1alpha5.DoNotConsolidateNodeAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can delete nodes, considers karpenter.sh/do-not-disrupt on nodes", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			nodeClaim2.Annotations = lo.Assign(nodeClaim2.Annotations, map[string]string{v1beta1.DoNotDisruptAnnotationKey: "true"})
			node2.Annotations = lo.Assign(nodeClaim2.Annotations, map[string]string{v1beta1.DoNotDisruptAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can delete nodes, considers karpenter.sh/do-not-evict on pods", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				}})
			// Block this pod from being disrupted with karpenter.sh/do-not-evict
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeClaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can delete nodes, considers karpenter.sh/do-not-disrupt on pods", func() {
			labels := map[string]string{
				"app": "test",
			}

			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			// Block this pod from being disrupted with karpenter.sh/do-not-disrupt
			pods[2].Annotations = lo.Assign(pods[2].Annotations, map[string]string{v1beta1.DoNotDisruptAnnotationKey: "true"})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim, node, nodeClaim2, node2)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

			// we should delete the non-annotated node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim, node)
		})
		It("can delete nodes, evicts pods without an ownerRef", func() {
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			// pod[2] is a stand-alone (non ReplicaSet) pod
			pods[2].OwnerReferences = nil
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool)

			// two pods on node 1
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			// one on node 2, but it's a standalone pod
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)

			// we don't need a new node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// but we expect to delete the nodeclaim with the fewest pods (nodeclaim 2) even though the pod has no ownerRefs
			// and will not be recreated
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		})
		It("won't delete node if it would require pods to schedule on an un-initialized node", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims, intentionally leaving node as not ready
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node2}, []*v1beta1.NodeClaim{nodeClaim2})

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// shouldn't delete the node
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))

			// Expect Unconsolidatable events to be fired
			evts := recorder.Events()
			_, ok := lo.Find(evts, func(e events.Event) bool {
				return strings.Contains(e.Message, "not all pods would schedule")
			})
			Expect(ok).To(BeTrue())
			_, ok = lo.Find(evts, func(e events.Event) bool {
				return strings.Contains(e.Message, "would schedule against a non-initialized node")
			})
			Expect(ok).To(BeTrue())
		})
		It("should consider initialized nodes before un-initialized nodes", func() {
			defaultInstanceType := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "default-instance-type",
				Resources: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("3"),
					v1.ResourceMemory: resource.MustParse("3Gi"),
					v1.ResourcePods:   resource.MustParse("110"),
				},
			})
			smallInstanceType := fake.NewInstanceType(fake.InstanceTypeOptions{
				Name: "small-instance-type",
				Resources: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
					v1.ResourcePods:   resource.MustParse("10"),
				},
			})
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				defaultInstanceType,
				smallInstanceType,
			}
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			podCount := 100
			pods := test.Pods(podCount, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("2"),
						v1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, rs, nodePool)

			// Setup 100 nodeclaims/nodes with a single nodeclaim/node that is initialized
			elem := rand.Intn(100) //nolint:gosec
			for i := 0; i < podCount; i++ {
				m, n := test.NodeClaimAndNode(v1beta1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							v1beta1.NodePoolLabelKey:     nodePool.Name,
							v1.LabelInstanceTypeStable:   defaultInstanceType.Name,
							v1beta1.CapacityTypeLabelKey: defaultInstanceType.Offerings[0].CapacityType,
							v1.LabelTopologyZone:         defaultInstanceType.Offerings[0].Zone,
						},
					},
					Status: v1beta1.NodeClaimStatus{
						ProviderID: test.RandomProviderID(),
						Allocatable: map[v1.ResourceName]resource.Quantity{
							v1.ResourceCPU:    resource.MustParse("3"),
							v1.ResourceMemory: resource.MustParse("3Gi"),
							v1.ResourcePods:   resource.MustParse("100"),
						},
					},
				})
				ExpectApplied(ctx, env.Client, pods[i], m, n)
				ExpectManualBinding(ctx, env.Client, pods[i], n)

				if i == elem {
					ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{n}, []*v1beta1.NodeClaim{m})
				} else {
					ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(m))
					ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
				}
			}

			// Create a pod and nodeclaim/node that will eventually be scheduled onto the initialized node
			consolidatableNodeClaim, consolidatableNode := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   smallInstanceType.Name,
						v1beta1.CapacityTypeLabelKey: smallInstanceType.Offerings[0].CapacityType,
						v1.LabelTopologyZone:         smallInstanceType.Offerings[0].Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
						v1.ResourcePods:   resource.MustParse("100"),
					},
				},
			})

			// create a new RS so we can link a pod to it
			rs = test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			consolidatablePod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, consolidatableNodeClaim, consolidatableNode, consolidatablePod)
			ExpectManualBinding(ctx, env.Client, consolidatablePod, consolidatableNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{consolidatableNode}, []*v1beta1.NodeClaim{consolidatableNodeClaim})

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, consolidatableNodeClaim)
			// Expect no events that state that the pods would schedule against a non-initialized node
			evts := recorder.Events()
			_, ok := lo.Find(evts, func(e events.Event) bool {
				return strings.Contains(e.Message, "would schedule against a non-initialized node")
			})
			Expect(ok).To(BeFalse())

			// the nodeclaim with the small instance should consolidate onto the initialized node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(100))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(100))
			ExpectNotFound(ctx, env.Client, consolidatableNodeClaim, consolidatableNode)
		})
		It("can delete nodes with a permanently pending pod", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			pending := test.UnschedulablePod(test.PodOptions{
				NodeSelector: map[string]string{
					"non-existent": "node-label",
				},
			})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool, pending)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)

			// pending pod is still here and hasn't been scheduled anywayre
			pending = ExpectPodExists(ctx, env.Client, pending.Name, pending.Namespace)
			Expect(pending.Spec.NodeName).To(BeEmpty())
		})
		It("won't delete nodes if it would make a non-pending pod go pending", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			// setup labels and node selectors so we force the pods onto the nodes we want
			node.Labels["foo"] = "1"
			node2.Labels["foo"] = "2"

			pods[0].Spec.NodeSelector = map[string]string{"foo": "1"}
			pods[1].Spec.NodeSelector = map[string]string{"foo": "1"}
			pods[2].Spec.NodeSelector = map[string]string{"foo": "2"}

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})

			// No node can be deleted as it would cause one of the three pods to go pending
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
		})
		It("can delete nodes while an invalid node pool exists", func() {
			labels := map[string]string{
				"app": "test",
			}

			// this invalid node pool should not be enough to stop all disruption
			badNodePool := &v1beta1.NodePool{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bad-nodepool",
				},
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						Spec: v1beta1.NodeClaimSpec{
							Requirements: []v1.NodeSelectorRequirement{},
							NodeClassRef: &v1beta1.NodeClassReference{
								Name: "non-existent",
							},
						},
					},
				},
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, badNodePool, rs, pods[0], pods[1], pods[2], nodeClaim, node, nodeClaim2, node2, nodePool)
			cloudProvider.ErrorsForNodePool[badNodePool.Name] = fmt.Errorf("unable to fetch instance types")

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)

			// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the old one
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		})
	})
	Context("TTL", func() {
		var nodeClaim1, nodeClaim2 *v1beta1.NodeClaim
		var node1, node2 *v1.Node

		BeforeEach(func() {
			nodeClaim1, node1 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaim2, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
		})
		It("should wait for the node TTL for empty nodes before consolidating", func() {
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// wait for the controller to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())
			// and the node should not be deleted yet
			ExpectExists(ctx, env.Client, nodeClaim1)

			// advance the clock so that the timeout expires
			fakeClock.Step(31 * time.Second)
			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)

			// nodeclaim should be deleted after the TTL due to emptiness
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(0))
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
		It("should wait for the node TTL for non-empty nodes before consolidating", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			// assign the nodeclaims to the least expensive offering so only one of them gets deleted
			nodeClaim1.Labels = lo.Assign(nodeClaim1.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})
			node1.Labels = lo.Assign(node1.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})
			nodeClaim2.Labels = lo.Assign(nodeClaim2.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})
			node2.Labels = lo.Assign(node2.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim1, node1, nodeClaim2, node2, nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], node1)
			ExpectManualBinding(ctx, env.Client, pods[1], node1)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2})

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, types.NamespacedName{})
			}()

			// wait for the controller to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())
			// and the node should not be deleted yet
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, nodeClaim2)

			// advance the clock so that the timeout expires
			fakeClock.Step(31 * time.Second)

			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim2)

			// nodeclaim should be deleted after the TTL due to emptiness
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2)
		})
		It("should not consolidate if the action picks different instance types after the node TTL wait", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("1"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool, pod)
			ExpectManualBinding(ctx, env.Client, pod, node1)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// wait for the disruptionController to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())

			// and the node should not be deleted yet
			ExpectExists(ctx, env.Client, node1)

			// add an additional pod to the node to change the consolidation decision
			pod2 := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("1"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, pod2)
			ExpectManualBinding(ctx, env.Client, pod2, node1)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			// advance the clock so that the timeout expires
			fakeClock.Step(31 * time.Second)
			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// nothing should be removed since the node is no longer empty
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, node1)
		})
		It("should not consolidate if the action becomes invalid during the node TTL wait", func() {
			pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
			}})
			ExpectApplied(ctx, env.Client, nodeClaim1, node1, nodePool, pod)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1}, []*v1beta1.NodeClaim{nodeClaim1})

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// wait for the disruptionController to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())
			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())
			// and the node should not be deleted yet
			ExpectExists(ctx, env.Client, nodeClaim1)

			// make the node non-empty by binding it
			ExpectManualBinding(ctx, env.Client, pod, node1)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

			// advance the clock so that the timeout expires
			fakeClock.Step(31 * time.Second)
			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// nothing should be removed since the node is no longer empty
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, nodeClaim1)
		})
		It("should not replace node if a pod schedules with karpenter.sh/do-not-evict during the TTL wait", func() {
			pod := test.Pod()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup

			// Trigger the reconcile loop to start but don't trigger the verify action
			wg.Add(1)
			go func() {
				defer wg.Done()
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// Iterate in a loop until we get to the validation action
			// Then, apply the pods to the cluster and bind them to the nodes
			for {
				time.Sleep(100 * time.Millisecond)
				if fakeClock.HasWaiters() {
					break
				}
			}
			doNotEvictPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha5.DoNotEvictPodAnnotationKey: "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, doNotEvictPod)
			ExpectManualBinding(ctx, env.Client, doNotEvictPod, node)

			// Step forward to satisfy the validation timeout and wait for the reconcile to finish
			ExpectTriggerVerifyAction(&wg)
			wg.Wait()

			// we would normally be able to replace a node, but we are blocked by the do-not-evict pods during validation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, node)
		})
		It("should not replace node if a pod schedules with karpenter.sh/do-not-disrupt during the TTL wait", func() {
			pod := test.Pod()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup

			// Trigger the reconcile loop to start but don't trigger the verify action
			wg.Add(1)
			go func() {
				defer wg.Done()
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// Iterate in a loop until we get to the validation action
			// Then, apply the pods to the cluster and bind them to the nodes
			for {
				time.Sleep(100 * time.Millisecond)
				if fakeClock.HasWaiters() {
					break
				}
			}
			doNotDisruptPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1beta1.DoNotDisruptAnnotationKey: "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, doNotDisruptPod)
			ExpectManualBinding(ctx, env.Client, doNotDisruptPod, node)

			// Step forward to satisfy the validation timeout and wait for the reconcile to finish
			ExpectTriggerVerifyAction(&wg)
			wg.Wait()

			// we would normally be able to replace a node, but we are blocked by the do-not-disrupt pods during validation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, node)
		})
		It("should not replace node if a pod schedules with a blocking PDB during the TTL wait", func() {
			pod := test.Pod()
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup

			// Trigger the reconcile loop to start but don't trigger the verify action
			wg.Add(1)
			go func() {
				defer wg.Done()
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// Iterate in a loop until we get to the validation action
			// Then, apply the pods to the cluster and bind them to the nodes
			for {
				time.Sleep(100 * time.Millisecond)
				if fakeClock.HasWaiters() {
					break
				}
			}
			labels := map[string]string{
				"app": "test",
			}
			blockingPDBPod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
			})
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         labels,
				MaxUnavailable: fromInt(0),
			})
			ExpectApplied(ctx, env.Client, blockingPDBPod, pdb)
			ExpectManualBinding(ctx, env.Client, blockingPDBPod, node)

			// Step forward to satisfy the validation timeout and wait for the reconcile to finish
			ExpectTriggerVerifyAction(&wg)
			wg.Wait()

			// we would normally be able to replace a node, but we are blocked by the PDB during validation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectExists(ctx, env.Client, node)
		})
		It("should not delete node if pods schedule with karpenter.sh/do-not-evict during the TTL wait", func() {
			pods := test.Pods(2, test.PodOptions{})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, nodeClaim2, node2, pods[0], pods[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup

			// Trigger the reconcile loop to start but don't trigger the verify action
			wg.Add(1)
			go func() {
				defer wg.Done()
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// Iterate in a loop until we get to the validation action
			// Then, apply the pods to the cluster and bind them to the nodes
			for {
				time.Sleep(100 * time.Millisecond)
				if fakeClock.HasWaiters() {
					break
				}
			}
			doNotEvictPods := test.Pods(2, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha5.DoNotEvictPodAnnotationKey: "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, doNotEvictPods[0], doNotEvictPods[1])
			ExpectManualBinding(ctx, env.Client, doNotEvictPods[0], node)
			ExpectManualBinding(ctx, env.Client, doNotEvictPods[1], node2)

			// Step forward to satisfy the validation timeout and wait for the reconcile to finish
			ExpectTriggerVerifyAction(&wg)
			wg.Wait()

			// we would normally be able to consolidate down to a single node, but we are blocked by the do-not-evict pods during validation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectExists(ctx, env.Client, node)
			ExpectExists(ctx, env.Client, node2)
		})
		It("should not delete node if pods schedule with karpenter.sh/do-not-disrupt during the TTL wait", func() {
			pods := test.Pods(2, test.PodOptions{})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, nodeClaim2, node2, pods[0], pods[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup

			// Trigger the reconcile loop to start but don't trigger the verify action
			wg.Add(1)
			go func() {
				defer wg.Done()
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// Iterate in a loop until we get to the validation action
			// Then, apply the pods to the cluster and bind them to the nodes
			for {
				time.Sleep(100 * time.Millisecond)
				if fakeClock.HasWaiters() {
					break
				}
			}
			doNotDisruptPods := test.Pods(2, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1beta1.DoNotDisruptAnnotationKey: "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, doNotDisruptPods[0], doNotDisruptPods[1])
			ExpectManualBinding(ctx, env.Client, doNotDisruptPods[0], node)
			ExpectManualBinding(ctx, env.Client, doNotDisruptPods[1], node2)

			// Step forward to satisfy the validation timeout and wait for the reconcile to finish
			ExpectTriggerVerifyAction(&wg)
			wg.Wait()

			// we would normally be able to consolidate down to a single node, but we are blocked by the do-not-disrupt pods during validation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectExists(ctx, env.Client, node)
			ExpectExists(ctx, env.Client, node2)
		})
		It("should not delete node if pods schedule with a blocking PDB during the TTL wait", func() {
			pods := test.Pods(2, test.PodOptions{})
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, nodeClaim2, node2, pods[0], pods[1])

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pods[0], node)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node, node2}, []*v1beta1.NodeClaim{nodeClaim, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup

			// Trigger the reconcile loop to start but don't trigger the verify action
			wg.Add(1)
			go func() {
				defer wg.Done()
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// Iterate in a loop until we get to the validation action
			// Then, apply the pods to the cluster and bind them to the nodes
			for {
				time.Sleep(100 * time.Millisecond)
				if fakeClock.HasWaiters() {
					break
				}
			}
			labels := map[string]string{
				"app": "test",
			}
			blockingPDBPods := test.Pods(2, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
			})
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         labels,
				MaxUnavailable: fromInt(0),
			})
			ExpectApplied(ctx, env.Client, blockingPDBPods[0], blockingPDBPods[1], pdb)
			ExpectManualBinding(ctx, env.Client, blockingPDBPods[0], node)
			ExpectManualBinding(ctx, env.Client, blockingPDBPods[1], node2)

			// Step forward to satisfy the validation timeout and wait for the reconcile to finish
			ExpectTriggerVerifyAction(&wg)
			wg.Wait()

			// we would normally be able to consolidate down to a single node, but we are blocked by the PDB during validation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectExists(ctx, env.Client, node)
			ExpectExists(ctx, env.Client, node2)
		})
	})
	Context("Timeout", func() {
		It("should return the last valid command when multi-nodeclaim consolidation times out", func() {
			numNodes := 20
			labels := map[string]string{
				"app": "test",
			}
			nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				}},
			)
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(numNodes, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						// Make the resource requests small so that many nodes can be consolidated at once.
						v1.ResourceCPU: resource.MustParse("10m"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, rs, nodePool)
			for _, nodeClaim := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaim)
			}
			for _, node := range nodes {
				ExpectApplied(ctx, env.Client, node)
			}
			for i, pod := range pods {
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, nodes[i])
			}

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// advance the clock so that the timeout expires
			fakeClock.Step(disruption.MultiNodeConsolidationTimeoutDuration)

			// wait for the controller to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*10).Should(BeTrue())

			ExpectTriggerVerifyAction(&wg)

			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())

			// and the node should not be deleted yet
			for i := range nodeClaims {
				ExpectExists(ctx, env.Client, nodeClaims[i])
			}

			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// should have at least two nodes deleted from multi nodeClaim consolidation
			Expect(len(ExpectNodeClaims(ctx, env.Client))).To(BeNumerically("<=", numNodes-2))
		})
		It("should exit single-nodeclaim consolidation if it times out", func() {
			numNodes := 25
			labels := map[string]string{
				"app": "test",
			}
			nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				}},
			)
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(numNodes, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						// Make the pods more than half of the allocatable so that only one nodeclaim can be done at any time
						v1.ResourceCPU: resource.MustParse("20"),
					},
				},
			})

			ExpectApplied(ctx, env.Client, rs, nodePool)
			for _, nodeClaim := range nodeClaims {
				ExpectApplied(ctx, env.Client, nodeClaim)
			}
			for _, node := range nodes {
				ExpectApplied(ctx, env.Client, node)
			}
			for i, pod := range pods {
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, nodes[i])
			}

			// inform cluster state about nodes and nodeClaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// advance the clock so that the timeout expires for multi-nodeClaim
			fakeClock.Step(disruption.MultiNodeConsolidationTimeoutDuration)
			// advance the clock so that the timeout expires for single-nodeClaim
			fakeClock.Step(disruption.SingleNodeConsolidationTimeoutDuration)

			ExpectTriggerVerifyAction(&wg)

			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// should have no nodeClaims deleted from single nodeClaim consolidation
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(numNodes))
		})
	})
	Context("Multi-NodeClaim", func() {
		var nodeClaim1, nodeClaim2, nodeClaim3 *v1beta1.NodeClaim
		var node1, node2, node3 *v1.Node

		BeforeEach(func() {
			nodeClaim1, node1 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaim2, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaim3, node3 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         mostExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
		})
		It("can merge 3 nodes into 1", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim1, node1, nodeClaim2, node2, nodeClaim3, node3, nodePool)
			ExpectMakeNodesInitialized(ctx, env.Client, node1, node2, node3)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], node1)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)
			ExpectManualBinding(ctx, env.Client, pods[2], node3)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2, node3}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2, nodeClaim3})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1, nodeClaim2, nodeClaim3)

			// three nodeclaims should be replaced with a single nodeclaim
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1, nodeClaim2, node2, nodeClaim3, node3)
		})
		It("won't merge 2 nodes into 1 of the same type", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			// Make the nodeclaims the least expensive instance type and make them of the same type
			nodeClaim1.Labels = lo.Assign(nodeClaim1.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})
			node1.Labels = lo.Assign(node1.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})
			nodeClaim2.Labels = lo.Assign(nodeClaim1.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})
			node2.Labels = lo.Assign(node2.Labels, map[string]string{
				v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
				v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
				v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
			})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim1, node1, nodeClaim2, node2, nodePool)
			ExpectMakeNodesInitialized(ctx, env.Client, node1, node2)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], node1)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)

			// We have [cheap-node, cheap-node] which multi-node consolidation could consolidate via
			// [delete cheap-node, delete cheap-node, launch cheap-node]. This isn't the best method though
			// as we should instead just delete one of the nodes instead of deleting both and launching a single
			// identical replacement. This test verifies the filterOutSameType function from multi-node consolidation
			// works to ensure we perform the least-disruptive action.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// should have just deleted the node with the fewest pods
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
			// and left the other node alone
			ExpectExists(ctx, env.Client, nodeClaim2)
			ExpectExists(ctx, env.Client, node2)
		})
		It("should wait for the node TTL for non-empty nodes before consolidating (multi-node)", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim1, node1, nodeClaim2, node2, nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], node1)
			ExpectManualBinding(ctx, env.Client, pods[1], node1)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2})

			var wg sync.WaitGroup
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)

			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// wait for the controller to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*5).Should(BeTrue())
			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())
			// and the node should not be deleted yet
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, nodeClaim2)

			// advance the clock so that the timeout expires
			fakeClock.Step(31 * time.Second)

			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1, nodeClaim2)

			// should launch a single smaller replacement node
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete the two large ones
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1, nodeClaim2, node2)
		})
		It("should continue to multi-nodeclaim consolidation when emptiness fails validation after the node ttl", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim1, node1, nodeClaim2, node2, nodeClaim3, node3, nodePool)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2, node3}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2, nodeClaim3})

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// wait for the controller to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*5).Should(BeTrue())
			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())
			// and the node should not be deleted yet
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, nodeClaim2)
			ExpectExists(ctx, env.Client, nodeClaim3)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], node1)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)
			ExpectManualBinding(ctx, env.Client, pods[2], node3)

			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node3))
			// advance the clock so that the timeout expires for emptiness
			Eventually(fakeClock.HasWaiters, time.Second*5).Should(BeTrue())
			fakeClock.Step(31 * time.Second)

			// Succeed on multi node consolidation
			Eventually(fakeClock.HasWaiters, time.Second*5).Should(BeTrue())
			fakeClock.Step(31 * time.Second)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1, nodeClaim2, nodeClaim3)

			// should have 2 nodes after multi nodeclaim consolidation deletes one
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			// and delete node3 in single nodeclaim consolidation
			ExpectNotFound(ctx, env.Client, nodeClaim2, node2, nodeClaim3, node3)
		})
		It("should continue to single nodeclaim consolidation when multi-nodeclaim consolidation fails validation after the node ttl", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodeClaim1, node1, nodeClaim2, node2, nodeClaim3, node3, nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], node1)
			ExpectManualBinding(ctx, env.Client, pods[1], node2)
			ExpectManualBinding(ctx, env.Client, pods[2], node3)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2, node3}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2, nodeClaim3})

			var wg sync.WaitGroup
			wg.Add(1)
			finished := atomic.Bool{}
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				defer finished.Store(true)
				ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			}()

			// wait for the controller to block on the validation timeout
			Eventually(fakeClock.HasWaiters, time.Second*5).Should(BeTrue())
			// controller should be blocking during the timeout
			Expect(finished.Load()).To(BeFalse())

			// and the node should not be deleted yet
			ExpectExists(ctx, env.Client, nodeClaim1)
			ExpectExists(ctx, env.Client, nodeClaim2)
			ExpectExists(ctx, env.Client, nodeClaim3)

			var extraPods []*v1.Pod
			for i := 0; i < 2; i++ {
				extraPods = append(extraPods, test.Pod(test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewQuantity(1, resource.DecimalSI)},
					},
				}))
			}
			ExpectApplied(ctx, env.Client, extraPods[0], extraPods[1])
			// bind the extra pods to node1 and node 2 to make the consolidation decision invalid
			// we bind to 2 nodes so we can deterministically expect that node3 is consolidated in
			// single nodeclaim consolidation
			ExpectManualBinding(ctx, env.Client, extraPods[0], node1)
			ExpectManualBinding(ctx, env.Client, extraPods[1], node2)

			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))

			// advance the clock so that the timeout expires for multi-nodeclaim consolidation
			fakeClock.Step(31 * time.Second)

			// wait for the controller to block on the validation timeout for single nodeclaim consolidation
			Eventually(fakeClock.HasWaiters, time.Second*5).Should(BeTrue())
			// advance the clock so that the timeout expires for single nodeclaim consolidation
			fakeClock.Step(31 * time.Second)

			// controller should finish
			Eventually(finished.Load, 10*time.Second).Should(BeTrue())
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1, nodeClaim2, nodeClaim3)

			// should have 2 nodes after single nodeclaim consolidation deletes one
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			// and delete node3 in single nodeclaim consolidation
			ExpectNotFound(ctx, env.Client, nodeClaim3, node3)
		})
	})
	Context("Node Lifetime Consideration", func() {
		var nodeClaim1, nodeClaim2 *v1beta1.NodeClaim
		var node1, node2 *v1.Node

		BeforeEach(func() {
			nodePool.Spec.Disruption.ExpireAfter = v1beta1.NillableDuration{Duration: lo.ToPtr(3 * time.Second)}
			nodeClaim1, node1 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
			nodeClaim2, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
						v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.CapacityType,
						v1.LabelTopologyZone:         leastExpensiveOffering.Zone,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:  resource.MustParse("32"),
						v1.ResourcePods: resource.MustParse("100"),
					},
				},
			})
		})
		It("should consider node lifetime remaining when calculating disruption cost", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			pods := test.Pods(3, test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], nodePool)
			ExpectApplied(ctx, env.Client, nodeClaim1, node1) // ensure node1 is the oldest node
			time.Sleep(2 * time.Second)                       // this sleep is unfortunate, but necessary.  The creation time is from etcd, and we can't mock it, so we
			// need to sleep to force the second node to be created a bit after the first node.
			ExpectApplied(ctx, env.Client, nodeClaim2, node2)

			// two pods on node 1, one on node 2
			ExpectManualBinding(ctx, env.Client, pods[0], node1)
			ExpectManualBinding(ctx, env.Client, pods[1], node1)
			ExpectManualBinding(ctx, env.Client, pods[2], node2)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node1, node2}, []*v1beta1.NodeClaim{nodeClaim1, nodeClaim2})

			fakeClock.SetTime(time.Now())

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})

			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim1)

			// the second node has more pods, so it would normally not be picked for consolidation, except it very little
			// lifetime remaining, so it should be deleted
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(1))
			ExpectNotFound(ctx, env.Client, nodeClaim1, node1)
		})
	})
	Context("Topology Consideration", func() {
		var zone1NodeClaim, zone2NodeClaim, zone3NodeClaim *v1beta1.NodeClaim
		var zone1Node, zone2Node, zone3Node *v1.Node
		var oldNodeClaimNames sets.Set[string]

		BeforeEach(func() {
			testZone1Instance := leastExpensiveInstanceWithZone("test-zone-1")
			testZone2Instance := mostExpensiveInstanceWithZone("test-zone-2")
			testZone3Instance := leastExpensiveInstanceWithZone("test-zone-3")

			zone1NodeClaim, zone1Node = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelTopologyZone:         "test-zone-1",
						v1.LabelInstanceTypeStable:   testZone1Instance.Name,
						v1beta1.CapacityTypeLabelKey: testZone1Instance.Offerings[0].CapacityType,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")},
				},
			})
			zone2NodeClaim, zone2Node = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelTopologyZone:         "test-zone-2",
						v1.LabelInstanceTypeStable:   testZone2Instance.Name,
						v1beta1.CapacityTypeLabelKey: testZone2Instance.Offerings[0].CapacityType,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")},
				},
			})
			zone3NodeClaim, zone3Node = test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelTopologyZone:         "test-zone-3",
						v1.LabelInstanceTypeStable:   testZone3Instance.Name,
						v1beta1.CapacityTypeLabelKey: testZone1Instance.Offerings[0].CapacityType,
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID:  test.RandomProviderID(),
					Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")},
				},
			})
			oldNodeClaimNames = sets.New(zone1NodeClaim.Name, zone2NodeClaim.Name, zone3NodeClaim.Name)
		})
		It("can replace node maintaining zonal topology spread", func() {
			labels := map[string]string{
				"app": "test-zonal-spread",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			tsc := v1.TopologySpreadConstraint{
				MaxSkew:           1,
				TopologyKey:       v1.LabelTopologyZone,
				WhenUnsatisfiable: v1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
			}
			pods := test.Pods(4, test.PodOptions{
				ResourceRequirements:      v1.ResourceRequirements{Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}},
				TopologySpreadConstraints: []v1.TopologySpreadConstraint{tsc},
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					}}})

			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], zone1NodeClaim, zone1Node, zone2NodeClaim, zone2Node, zone3NodeClaim, zone3Node, nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], zone1Node)
			ExpectManualBinding(ctx, env.Client, pods[1], zone2Node)
			ExpectManualBinding(ctx, env.Client, pods[2], zone3Node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{zone1Node, zone2Node, zone3Node}, []*v1beta1.NodeClaim{zone1NodeClaim, zone2NodeClaim, zone3NodeClaim})

			ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))

			fakeClock.Step(10 * time.Minute)

			// consolidation won't delete the old node until the new node is ready
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// Process the item so that the nodes can be deleted.
			ExpectReconcileSucceeded(ctx, queue, types.NamespacedName{})
			// Cascade any deletion of the nodeclaim to the node
			ExpectNodeClaimsCascadeDeletion(ctx, env.Client, zone2NodeClaim)

			// should create a new node as there is a cheaper one that can hold the pod
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(3))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
			ExpectNotFound(ctx, env.Client, zone2NodeClaim, zone2Node)

			// Find the new node associated with the nodeclaim
			newNodeClaim, ok := lo.Find(ExpectNodeClaims(ctx, env.Client), func(m *v1beta1.NodeClaim) bool {
				return !oldNodeClaimNames.Has(m.Name)
			})
			Expect(ok).To(BeTrue())
			newNode, ok := lo.Find(ExpectNodes(ctx, env.Client), func(n *v1.Node) bool {
				return newNodeClaim.Status.ProviderID == n.Spec.ProviderID
			})
			Expect(ok).To(BeTrue())

			// we need to emulate the replicaset controller and bind a new pod to the newly created node
			ExpectApplied(ctx, env.Client, pods[3])
			ExpectManualBinding(ctx, env.Client, pods[3], newNode)

			// we should maintain our skew, the new node must be in the same zone as the old node it replaced
			ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))
		})
		It("won't delete node if it would violate pod anti-affinity", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)
			pods := test.Pods(3, test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}},
				PodAntiRequirements: []v1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						TopologyKey:   v1.LabelHostname,
					},
				},
				ObjectMeta: metav1.ObjectMeta{Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
			})

			// Make the Zone 2 instance also the least expensive instance
			zone2Instance := leastExpensiveInstanceWithZone("test-zone-2")
			zone2Node.Labels = lo.Assign(zone2Node.Labels, map[string]string{
				v1beta1.NodePoolLabelKey:     nodePool.Name,
				v1.LabelTopologyZone:         "test-zone-2",
				v1.LabelInstanceTypeStable:   zone2Instance.Name,
				v1beta1.CapacityTypeLabelKey: zone2Instance.Offerings[0].CapacityType,
			})
			zone2NodeClaim.Labels = lo.Assign(zone2NodeClaim.Labels, map[string]string{
				v1beta1.NodePoolLabelKey:     nodePool.Name,
				v1.LabelTopologyZone:         "test-zone-2",
				v1.LabelInstanceTypeStable:   zone2Instance.Name,
				v1beta1.CapacityTypeLabelKey: zone2Instance.Offerings[0].CapacityType,
			})
			ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], zone1NodeClaim, zone1Node, zone2NodeClaim, zone2Node, zone3NodeClaim, zone3Node, nodePool)

			// bind pods to nodes
			ExpectManualBinding(ctx, env.Client, pods[0], zone1Node)
			ExpectManualBinding(ctx, env.Client, pods[1], zone2Node)
			ExpectManualBinding(ctx, env.Client, pods[2], zone3Node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{zone1Node, zone2Node, zone3Node}, []*v1beta1.NodeClaim{zone1NodeClaim, zone2NodeClaim, zone3NodeClaim})

			fakeClock.Step(10 * time.Minute)

			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
			wg.Wait()

			// our nodes are already the cheapest available, so we can't replace them.  If we delete, it would
			// violate the anti-affinity rule, so we can't do anything.
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(3))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(3))
			ExpectExists(ctx, env.Client, zone1NodeClaim)
			ExpectExists(ctx, env.Client, zone2NodeClaim)
			ExpectExists(ctx, env.Client, zone3NodeClaim)
		})
	})
	Context("Parallelization", func() {
		It("should schedule an additional node when receiving pending pods while consolidating", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			pod := test.Pod(test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
			})

			node.Finalizers = []string{"karpenter.sh/test-finalizer"}
			nodeClaim.Finalizers = []string{"karpenter.sh/test-finalizer"}

			ExpectApplied(ctx, env.Client, rs, pod, nodeClaim, node, nodePool)

			// bind pods to node
			ExpectManualBinding(ctx, env.Client, pod, node)

			// inform cluster state about nodes and nodeclaims
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

			fakeClock.Step(10 * time.Minute)

			// Run the processing loop in parallel in the background with environment context
			var wg sync.WaitGroup
			ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
			ExpectTriggerVerifyAction(&wg)
			go func() {
				defer GinkgoRecover()
				_, _ = disruptionController.Reconcile(ctx, reconcile.Request{})
			}()
			wg.Wait()

			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))

			// Add a new pending pod that should schedule while node is not yet deleted
			pod = test.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
			Expect(ExpectNodes(ctx, env.Client)).To(HaveLen(2))
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should not consolidate a node that is launched for pods on a deleting node", func() {
			labels := map[string]string{
				"app": "test",
			}
			// create our RS so we can link a pod to it
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			podOpts := test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "ReplicaSet",
							Name:               rs.Name,
							UID:                rs.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU: resource.MustParse("1"),
					},
				},
			}

			var pods []*v1.Pod
			for i := 0; i < 5; i++ {
				pod := test.UnschedulablePod(podOpts)
				pods = append(pods, pod)
			}
			ExpectApplied(ctx, env.Client, rs, nodePool)
			ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, lo.Map(pods, func(p *v1.Pod, _ int) *v1.Pod { return p.DeepCopy() })...)

			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			nodes := ExpectNodes(ctx, env.Client)
			Expect(nodes).To(HaveLen(1))

			// Update cluster state with new node
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(nodes[0]))

			// Mark the node for deletion and re-trigger reconciliation
			oldNodeName := nodes[0].Name
			cluster.MarkForDeletion(nodes[0].Spec.ProviderID)
			ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, prov, lo.Map(pods, func(p *v1.Pod, _ int) *v1.Pod { return p.DeepCopy() })...)

			// Make sure that the cluster state is aware of the current node state
			nodes = ExpectNodes(ctx, env.Client)
			Expect(nodes).To(HaveLen(2))
			newNode, _ := lo.Find(nodes, func(n *v1.Node) bool { return n.Name != oldNodeName })

			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nil)

			// Wait for the nomination cache to expire
			time.Sleep(time.Second * 11)

			// Re-create the pods to re-bind them
			for i := 0; i < 2; i++ {
				ExpectDeleted(ctx, env.Client, pods[i])
				pod := test.UnschedulablePod(podOpts)
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, newNode)
			}

			// Trigger a reconciliation run which should take into account the deleting node
			// consolidation shouldn't trigger additional actions
			fakeClock.Step(10 * time.Minute)
			var wg sync.WaitGroup
			ExpectTriggerVerifyAction(&wg)

			result, err := disruptionController.Reconcile(ctx, reconcile.Request{})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		})
	})
})