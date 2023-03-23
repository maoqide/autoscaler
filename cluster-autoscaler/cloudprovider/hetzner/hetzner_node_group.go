/*
Copyright 2019 The Kubernetes Authors.

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

package hetzner

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/hetzner/hcloud-go/hcloud"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/klog/v2"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

// hetznerNodeGroup implements cloudprovider.NodeGroup interface. hetznerNodeGroup contains
// configuration info and functions to control a set of nodes that have the
// same capacity and set of labels.
type hetznerNodeGroup struct {
	id           string
	manager      *hetznerManager
	minSize      int
	maxSize      int
	targetSize   int
	region       string
	instanceType string

	clusterUpdateMutex *sync.Mutex
}

type hetznerNodeGroupSpec struct {
	name         string
	minSize      int
	maxSize      int
	region       string
	instanceType string
}

// MaxSize returns maximum size of the node group.
func (n *hetznerNodeGroup) MaxSize() int {
	return n.maxSize
}

// MinSize returns minimum size of the node group.
func (n *hetznerNodeGroup) MinSize() int {
	return n.minSize
}

// GetOptions returns NodeGroupAutoscalingOptions that should be used for this particular
// NodeGroup. Returning a nil will result in using default options.
func (n *hetznerNodeGroup) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	return nil, cloudprovider.ErrNotImplemented
}

// TargetSize returns the current target size of the node group. It is possible
// that the number of nodes in Kubernetes is different at the moment but should
// be equal to Size() once everything stabilizes (new nodes finish startup and
// registration or removed nodes are deleted completely). Implementation
// required.
func (n *hetznerNodeGroup) TargetSize() (int, error) {
	return n.targetSize, nil
}

// IncreaseSize increases the size of the node group. To delete a node you need
// to explicitly name it and use DeleteNode. This function should wait until
// node group size is updated. Implementation required.
func (n *hetznerNodeGroup) IncreaseSize(delta int) error {
	if delta <= 0 {
		return fmt.Errorf("delta must be positive, have: %d", delta)
	}

	targetSize := n.targetSize + delta
	if targetSize > n.MaxSize() {
		return fmt.Errorf("size increase is too large. current: %d desired: %d max: %d", n.targetSize, targetSize, n.MaxSize())
	}

	klog.V(4).Infof("Scaling Instance Pool %s to %d", n.id, targetSize)

	n.clusterUpdateMutex.Lock()
	defer n.clusterUpdateMutex.Unlock()

	available, err := serverTypeAvailable(n.manager, n.instanceType, n.region)
	if err != nil {
		return fmt.Errorf("failed to check if type %s is available in region %s error: %v", n.instanceType, n.region, err)
	}
	if !available {
		return fmt.Errorf("server type %s not available in region %s", n.instanceType, n.region)
	}

	waitGroup := sync.WaitGroup{}
	for i := 0; i < delta; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			err := createServer(n)
			if err != nil {
				targetSize--
				klog.Errorf("failed to create error: %v", err)
			}
		}()
	}
	waitGroup.Wait()

	n.targetSize = targetSize

	// create new servers cache
	if _, err := n.manager.cachedServers.servers(); err != nil {
		klog.Errorf("failed to get servers: %v", err)
	}

	return nil
}

// DeleteNodes deletes nodes from this node group (and also increasing the size
// of the node group with that). Error is returned either on failure or if the
// given node doesn't belong to this node group. This function should wait
// until node group size is updated. Implementation required.
func (n *hetznerNodeGroup) DeleteNodes(nodes []*apiv1.Node) error {
	n.clusterUpdateMutex.Lock()
	defer n.clusterUpdateMutex.Unlock()

	targetSize := n.targetSize - len(nodes)
	if targetSize < n.MinSize() {
		return fmt.Errorf("size decrease is too large. current: %d desired: %d min: %d", n.targetSize, targetSize, n.MinSize())
	}

	waitGroup := sync.WaitGroup{}

	for _, node := range nodes {
		waitGroup.Add(1)
		go func(node *apiv1.Node) {
			klog.Infof("Evicting server %s", node.Name)

			err := n.manager.deleteByNode(node)
			if err != nil {
				klog.Errorf("failed to delete server ID %d error: %v", node.Name, err)
			}

			waitGroup.Done()
		}(node)
	}
	waitGroup.Wait()

	// create new servers cache
	if _, err := n.manager.cachedServers.servers(); err != nil {
		klog.Errorf("failed to get servers: %v", err)
	}

	n.resetTargetSize(-len(nodes))

	return nil
}

// DecreaseTargetSize decreases the target size of the node group. This function
// doesn't permit to delete any existing node and can be used only to reduce the
// request for new nodes that have not been yet fulfilled. Delta should be negative.
// It is assumed that cloud provider will not delete the existing nodes when there
// is an option to just decrease the target. Implementation required.
func (n *hetznerNodeGroup) DecreaseTargetSize(delta int) error {
	n.targetSize = n.targetSize + delta
	return nil
}

// Id returns an unique identifier of the node group.
func (n *hetznerNodeGroup) Id() string {
	return n.id
}

// Debug returns a string containing all information regarding this node group.
func (n *hetznerNodeGroup) Debug() string {
	return fmt.Sprintf("cluster ID: %s (min:%d max:%d)", n.Id(), n.MinSize(), n.MaxSize())
}

// Nodes returns a list of all nodes that belong to this node group.  It is
// required that Instance objects returned by this method have Id field set.
// Other fields are optional.
func (n *hetznerNodeGroup) Nodes() ([]cloudprovider.Instance, error) {
	servers, err := n.manager.cachedServers.getServersByNodeGroupName(n.id)
	if err != nil {
		return nil, fmt.Errorf("failed to get servers for hcloud: %v", err)
	}

	instances := make([]cloudprovider.Instance, 0, len(servers))
	for _, vm := range servers {
		instances = append(instances, toInstance(vm))
	}

	return instances, nil
}

// TemplateNodeInfo returns a schedulerframework.NodeInfo structure of an empty
// (as if just started) node. This will be used in scale-up simulations to
// predict what would a new node look like if a node group was expanded. The
// returned NodeInfo is expected to have a fully populated Node object, with
// all of the labels, capacity and allocatable information as well as all pods
// that are started on the node by default, using manifest (most likely only
// kube-proxy). Implementation optional.
func (n *hetznerNodeGroup) TemplateNodeInfo() (*schedulerframework.NodeInfo, error) {
	resourceList, err := getMachineTypeResourceList(n.manager, n.instanceType)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource list for node group %s error: %v", n.id, err)
	}

	node := apiv1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   newNodeName(n),
			Labels: map[string]string{},
		},
		Status: apiv1.NodeStatus{
			Capacity:   resourceList,
			Conditions: cloudprovider.BuildReadyConditions(),
		},
	}
	node.Status.Allocatable = node.Status.Capacity
	node.Status.Conditions = cloudprovider.BuildReadyConditions()

	nodeGroupLabels, err := buildNodeGroupLabels(n)
	if err != nil {
		return nil, err
	}
	node.Labels = cloudprovider.JoinStringMaps(node.Labels, nodeGroupLabels)

	nodeInfo := schedulerframework.NewNodeInfo(cloudprovider.BuildKubeProxy(n.id))
	nodeInfo.SetNode(&node)

	return nodeInfo, nil
}

// Exist checks if the node group really exists on the cloud provider side.
// Allows to tell the theoretical node group from the real one. Implementation
// required.
func (n *hetznerNodeGroup) Exist() bool {
	_, exists := n.manager.nodeGroups[n.id]
	return exists
}

// Create creates the node group on the cloud provider side. Implementation
// optional.
func (n *hetznerNodeGroup) Create() (cloudprovider.NodeGroup, error) {
	n.manager.nodeGroups[n.id] = n

	return n, cloudprovider.ErrNotImplemented
}

// Delete deletes the node group on the cloud provider side.  This will be
// executed only for autoprovisioned node groups, once their size drops to 0.
// Implementation optional.
func (n *hetznerNodeGroup) Delete() error {
	// We do not use actual node groups but all nodes within the Hcloud project are labeled with a group
	return nil
}

// Autoprovisioned returns true if the node group is autoprovisioned. An
// autoprovisioned group was created by CA and can be deleted when scaled to 0.
func (n *hetznerNodeGroup) Autoprovisioned() bool {
	// All groups are auto provisioned
	return false
}

func toInstance(vm *hcloud.Server) cloudprovider.Instance {
	return cloudprovider.Instance{
		Id:     toProviderID(vm.ID),
		Status: toInstanceStatus(vm.Status),
	}
}

func toProviderID(nodeID int) string {
	return fmt.Sprintf("%s%d", providerIDPrefix, nodeID)
}

func toInstanceStatus(status hcloud.ServerStatus) *cloudprovider.InstanceStatus {
	if status == "" {
		return nil
	}

	st := &cloudprovider.InstanceStatus{}
	switch status {
	case hcloud.ServerStatusInitializing:
	case hcloud.ServerStatusStarting:
		st.State = cloudprovider.InstanceCreating
	case hcloud.ServerStatusRunning:
		st.State = cloudprovider.InstanceRunning
	case hcloud.ServerStatusOff:
	case hcloud.ServerStatusDeleting:
	case hcloud.ServerStatusStopping:
		st.State = cloudprovider.InstanceDeleting
	default:
		st.ErrorInfo = &cloudprovider.InstanceErrorInfo{
			ErrorClass:   cloudprovider.OtherErrorClass,
			ErrorCode:    "no-code-hcloud",
			ErrorMessage: "error",
		}
	}

	return st
}

func newNodeName(n *hetznerNodeGroup) string {
	return fmt.Sprintf("%s-%x", n.id, rand.Int63())
}

func buildNodeGroupLabels(n *hetznerNodeGroup) (map[string]string, error) {
	archLabel, err := instanceTypeArch(n.manager, n.instanceType)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		apiv1.LabelInstanceType:      n.instanceType,
		apiv1.LabelTopologyRegion:    n.region,
		apiv1.LabelArchStable:        archLabel,
		"csi.hetzner.cloud/location": n.region,
		nodeGroupLabel:               n.id,
	}, nil
}

func getMachineTypeResourceList(m *hetznerManager, instanceType string) (apiv1.ResourceList, error) {
	typeInfo, err := m.cachedServerType.getServerType(instanceType)
	if err != nil || typeInfo == nil {
		return nil, fmt.Errorf("failed to get machine type %s info error: %v", instanceType, err)
	}

	return apiv1.ResourceList{
		// TODO somehow determine the actual pods that will be running
		apiv1.ResourcePods:    *resource.NewQuantity(defaultPodAmountsLimit, resource.DecimalSI),
		apiv1.ResourceCPU:     *resource.NewQuantity(int64(typeInfo.Cores), resource.DecimalSI),
		apiv1.ResourceMemory:  *resource.NewQuantity(int64(typeInfo.Memory*1024*1024*1024), resource.DecimalSI),
		apiv1.ResourceStorage: *resource.NewQuantity(int64(typeInfo.Disk*1024*1024*1024), resource.DecimalSI),
	}, nil
}

func serverTypeAvailable(manager *hetznerManager, instanceType string, region string) (bool, error) {
	serverType, err := manager.cachedServerType.getServerType(instanceType)
	if err != nil {
		return false, err
	}

	for _, price := range serverType.Pricings {
		if price.Location.Name == region {
			return true, nil
		}
	}

	return false, nil
}

func instanceTypeArch(manager *hetznerManager, instanceType string) (string, error) {
	serverType, err := manager.cachedServerType.getServerType(instanceType)
	if err != nil {
		return "", err
	}

	switch serverType.Architecture {
	case hcloud.ArchitectureARM:
		return "arm64", nil
	case hcloud.ArchitectureX86:
		return "amd64", nil
	default:
		return "amd64", nil
	}
}

func createServer(n *hetznerNodeGroup) error {
	StartAfterCreate := true
	opts := hcloud.ServerCreateOpts{
		Name:             newNodeName(n),
		UserData:         n.manager.cloudInit,
		Location:         &hcloud.Location{Name: n.region},
		ServerType:       &hcloud.ServerType{Name: n.instanceType},
		Image:            n.manager.image,
		StartAfterCreate: &StartAfterCreate,
		Labels: map[string]string{
			nodeGroupLabel: n.id,
		},
		PublicNet: &hcloud.ServerCreatePublicNet{
			EnableIPv4: n.manager.publicIPv4,
			EnableIPv6: n.manager.publicIPv6,
		},
	}
	if n.manager.sshKey != nil {
		opts.SSHKeys = []*hcloud.SSHKey{n.manager.sshKey}
	}
	if n.manager.network != nil {
		opts.Networks = []*hcloud.Network{n.manager.network}
	}
	if n.manager.firewall != nil {
		serverCreateFirewall := &hcloud.ServerCreateFirewall{Firewall: *n.manager.firewall}
		opts.Firewalls = []*hcloud.ServerCreateFirewall{serverCreateFirewall}
	}

	serverCreateResult, _, err := n.manager.client.Server.Create(n.manager.apiCallContext, opts)
	if err != nil {
		return fmt.Errorf("could not create server type %s in region %s: %v", n.instanceType, n.region, err)
	}

	action := serverCreateResult.Action
	server := serverCreateResult.Server
	err = waitForServerAction(n.manager, server.Name, action)
	if err != nil {
		_ = n.manager.deleteServer(server)
		return fmt.Errorf("failed to start server %s error: %v", server.Name, err)
	}

	return nil
}

func waitForServerAction(m *hetznerManager, serverName string, action *hcloud.Action) error {
	// The implementation of the Hetzner Cloud action client's WatchProgress
	// method may be a little puzzling. The following comment thus explains how
	// waitForServerAction works.
	//
	// WatchProgress returns two channels. The first channel is used to send a
	// ballpark estimate for the action progress, the second to send any error
	// that may occur.
	//
	// WatchProgress is implemented in such a way, that the first channel can
	// be ignored. It is not necessary to consume it to avoid a deadlock in
	// WatchProgress. Any write to this channel is wrapped in a select.
	// Progress updates are simply not sent if nothing reads from the other
	// side.
	//
	// Once the action completes successfully nil is send through the second
	// channel. Then both channels are closed.
	//
	// The following code therefore only watches the second channel. If it
	// reads an error from the channel the action is failed. Otherwise the
	// action is successful.
	_, errChan := m.client.Action.WatchProgress(m.apiCallContext, action)
	select {
	case err := <-errChan:
		if err != nil {
			return fmt.Errorf("error while waiting for server action: %s: %v", serverName, err)
		}
		return nil
	case <-time.After(m.createTimeout):
		return fmt.Errorf("timeout waiting for server %s", serverName)
	}
}

func (n *hetznerNodeGroup) resetTargetSize(expectedDelta int) {
	servers, err := n.manager.allServers(n.id)
	if err != nil {
		klog.Errorf("failed to set node pool %s size, using delta %d error: %v", n.id, expectedDelta, err)
		n.targetSize = n.targetSize - expectedDelta
	} else {
		klog.Infof("Set node group %s size from %d to %d, expected delta %d", n.id, n.targetSize, len(servers), expectedDelta)
		n.targetSize = len(servers)
	}
}
