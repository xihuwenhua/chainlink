package devenv

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/AlekSi/pointer"
	"github.com/hashicorp/go-multierror"
	"github.com/rs/zerolog"
	"github.com/sethvargo/go-retry"
	chainsel "github.com/smartcontractkit/chain-selectors"

	clclient "github.com/smartcontractkit/chainlink/integration-tests/client"
	nodev1 "github.com/smartcontractkit/chainlink/integration-tests/deployment/jd/node/v1"
	"github.com/smartcontractkit/chainlink/integration-tests/deployment/jd/shared/ptypes"
	"github.com/smartcontractkit/chainlink/integration-tests/web/sdk/client"
)

const (
	NodeLabelKeyType        = "type"
	NodeLabelValueBootstrap = "bootstrap"
	NodeLabelValuePlugin    = "plugin"
)

// NodeInfo holds the information required to create a node
type NodeInfo struct {
	CLConfig    clclient.ChainlinkConfig // config to connect to chainlink node via API
	P2PPort     string                   // port for P2P communication
	IsBootstrap bool                     // denotes if the node is a bootstrap node
	Name        string                   // name of the node, used to identify the node, helpful in logs
	AdminAddr   string                   // admin address to send payments to, applicable only for non-bootstrap nodes
}

type DON struct {
	Nodes []Node
}

func (don *DON) PluginNodes() []Node {
	var pluginNodes []Node
	for _, node := range don.Nodes {
		for _, label := range node.labels {
			if label.Key == NodeLabelKeyType && pointer.GetString(label.Value) == NodeLabelValuePlugin {
				pluginNodes = append(pluginNodes, node)
			}
		}
	}
	return pluginNodes
}

// ReplayAllLogs replays all logs for the chains on all nodes for given block numbers for each chain
func (don *DON) ReplayAllLogs(blockbyChain map[uint64]uint64) error {
	for _, node := range don.Nodes {
		if err := node.ReplayLogs(blockbyChain); err != nil {
			return err
		}
	}
	return nil
}

func (don *DON) NodeIds() []string {
	var nodeIds []string
	for _, node := range don.Nodes {
		nodeIds = append(nodeIds, node.NodeId)
	}
	return nodeIds
}

func (don *DON) CreateSupportedChains(ctx context.Context, chains []ChainConfig, jd JobDistributor) error {
	var err error
	for i := range don.Nodes {
		node := &don.Nodes[i]
		if err1 := node.CreateCCIPOCRSupportedChains(ctx, chains, jd); err1 != nil {
			err = multierror.Append(err, err1)
		}
		don.Nodes[i] = *node
	}
	return err
}

// NewRegisteredDON creates a DON with the given node info, registers the nodes with the job distributor
// and sets up the job distributor in the nodes
func NewRegisteredDON(ctx context.Context, nodeInfo []NodeInfo, jd JobDistributor) (*DON, error) {
	don := &DON{
		Nodes: make([]Node, 0),
	}
	for i, info := range nodeInfo {
		if info.Name == "" {
			info.Name = fmt.Sprintf("node-%d", i)
		}
		node, err := NewNode(info)
		if err != nil {
			return nil, fmt.Errorf("failed to create node %d: %w", i, err)
		}
		// node Labels so that it's easier to query them
		if info.IsBootstrap {
			// create multi address for OCR2, applicable only for bootstrap nodes

			node.multiAddr = fmt.Sprintf("%s:%s", info.CLConfig.InternalIP, info.P2PPort)
			// no need to set admin address for bootstrap nodes, as there will be no payment
			node.adminAddr = ""
			node.labels = append(node.labels, &ptypes.Label{
				Key:   NodeLabelKeyType,
				Value: pointer.ToString(NodeLabelValueBootstrap),
			})
		} else {
			// multi address is not applicable for non-bootstrap nodes
			// explicitly set it to empty string to denote that
			node.multiAddr = ""
			node.labels = append(node.labels, &ptypes.Label{
				Key:   NodeLabelKeyType,
				Value: pointer.ToString(NodeLabelValuePlugin),
			})
		}
		// Set up Job distributor in node and register node with the job distributor
		err = node.SetUpAndLinkJobDistributor(ctx, jd)
		if err != nil {
			return nil, fmt.Errorf("failed to set up job distributor in node %s: %w", info.Name, err)
		}

		don.Nodes = append(don.Nodes, *node)
	}
	return don, nil
}

func NewNode(nodeInfo NodeInfo) (*Node, error) {
	gqlClient, err := client.New(nodeInfo.CLConfig.URL, client.Credentials{
		Email:    nodeInfo.CLConfig.Email,
		Password: nodeInfo.CLConfig.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create node graphql client: %w", err)
	}
	chainlinkClient, err := clclient.NewChainlinkClient(&nodeInfo.CLConfig, zerolog.Logger{})
	if err != nil {
		return nil, fmt.Errorf("failed to create node rest client: %w", err)
	}
	return &Node{
		gqlClient:  gqlClient,
		restClient: chainlinkClient,
		Name:       nodeInfo.Name,
		adminAddr:  nodeInfo.AdminAddr,
	}, nil
}

type Node struct {
	NodeId      string                    // node id returned by job distributor after node is registered with it
	JDId        string                    // job distributor id returned by node after Job distributor is created in node
	Name        string                    // name of the node
	AccountAddr map[uint64]string         // chain selector to node's account address mapping for supported chains
	gqlClient   client.Client             // graphql client to interact with the node
	restClient  *clclient.ChainlinkClient // rest client to interact with the node
	labels      []*ptypes.Label           // labels with which the node is registered with the job distributor
	adminAddr   string                    // admin address to send payments to, applicable only for non-bootstrap nodes
	multiAddr   string                    // multi address denoting node's FQN (needed for deriving P2PBootstrappers in OCR), applicable only for bootstrap nodes
}

// CreateCCIPOCRSupportedChains creates a JobDistributorChainConfig for the node.
// It works under assumption that the node is already registered with the job distributor.
// It expects bootstrap nodes to have label with key "type" and value as "bootstrap".
// It fetches the account address, peer id, and OCR2 key bundle id and creates the JobDistributorChainConfig.
func (n *Node) CreateCCIPOCRSupportedChains(ctx context.Context, chains []ChainConfig, jd JobDistributor) error {
	for i, chain := range chains {
		chainId := strconv.FormatUint(chain.ChainID, 10)
		accountAddr, err := n.gqlClient.FetchAccountAddress(ctx, chainId)
		if err != nil {
			return fmt.Errorf("failed to fetch account address for node %s: %w", n.Name, err)
		}
		if accountAddr == nil {
			return fmt.Errorf("no account address found for node %s", n.Name)
		}
		if n.AccountAddr == nil {
			n.AccountAddr = make(map[uint64]string)
		}
		n.AccountAddr[chain.ChainID] = *accountAddr
		peerID, err := n.gqlClient.FetchP2PPeerID(ctx)
		if err != nil {
			return fmt.Errorf("failed to fetch peer id for node %s: %w", n.Name, err)
		}
		if peerID == nil {
			return fmt.Errorf("no peer id found for node %s", n.Name)
		}

		ocr2BundleId, err := n.gqlClient.FetchOCR2KeyBundleID(ctx, chain.ChainType)
		if err != nil {
			return fmt.Errorf("failed to fetch OCR2 key bundle id for node %s: %w", n.Name, err)
		}
		if ocr2BundleId == "" {
			return fmt.Errorf("no OCR2 key bundle id found for node %s", n.Name)
		}
		// fetch node labels to know if the node is bootstrap or plugin
		isBootstrap := false
		for _, label := range n.labels {
			if label.Key == NodeLabelKeyType && pointer.GetString(label.Value) == NodeLabelValueBootstrap {
				isBootstrap = true
				break
			}
		}
		// JD silently fails to update nodeChainConfig. Therefore, we fetch the node config and
		// if it's not updated , we retry creating the chain config.
		// as a workaround, we keep trying creating the chain config for 5 times until it's created
		retryCount := 1
		created := false
		for retryCount < 5 {
			chainConfigId, err := n.gqlClient.CreateJobDistributorChainConfig(ctx, client.JobDistributorChainConfigInput{
				JobDistributorID: n.JDId,
				ChainID:          chainId,
				ChainType:        chain.ChainType,
				AccountAddr:      pointer.GetString(accountAddr),
				AdminAddr:        n.adminAddr,
				Ocr2Enabled:      true,
				Ocr2IsBootstrap:  isBootstrap,
				Ocr2Multiaddr:    n.multiAddr,
				Ocr2P2PPeerID:    pointer.GetString(peerID),
				Ocr2KeyBundleID:  ocr2BundleId,
				Ocr2Plugins:      `{"commit":true,"execute":true,"median":false,"mercury":false}`,
			})
			if err != nil {
				return fmt.Errorf("failed to create CCIPOCR2SupportedChains for node %s: %w", n.Name, err)
			}
			// JD doesn't update the node chain config immediately, so we need to wait for it to be updated
			err = retry.Do(ctx, retry.WithMaxRetries(3, retry.NewFibonacci(1*time.Second)), func(ctx context.Context) error {
				nodeChainConfigs, err := jd.ListNodeChainConfigs(context.Background(), &nodev1.ListNodeChainConfigsRequest{
					Filter: &nodev1.ListNodeChainConfigsRequest_Filter{
						NodeIds: []string{n.NodeId},
					}})
				if err != nil {
					return fmt.Errorf("failed to list node chain configs for node %s: %w", n.Name, err)
				}
				if nodeChainConfigs != nil && len(nodeChainConfigs.ChainConfigs) == i+1 {
					return nil
				}
				return fmt.Errorf("node chain config not updated properly")
			})
			if err == nil {
				created = true
				break
			}
			// delete the node chain config if it's not updated properly and retry
			err = n.gqlClient.DeleteJobDistributorChainConfig(ctx, chainConfigId)
			if err != nil {
				return fmt.Errorf("failed to delete job distributor chain config for node %s: %w", n.Name, err)
			}

			retryCount++
		}
		if !created {
			return fmt.Errorf("failed to create CCIPOCR2SupportedChains for node %s", n.Name)
		}
	}
	return nil
}

// AcceptJob accepts the job proposal for the given job proposal spec
func (n *Node) AcceptJob(ctx context.Context, spec string) error {
	// fetch JD to get the job proposals
	jd, err := n.gqlClient.GetJobDistributor(ctx, n.JDId)
	if err != nil {
		return err
	}
	if jd.GetJobProposals() == nil {
		return fmt.Errorf("no job proposals found for node %s", n.Name)
	}
	// locate the job proposal id for the given job spec
	var idToAccept string
	for _, jp := range jd.JobProposals {
		if jp.LatestSpec.Definition == spec {
			idToAccept = jp.Id
			break
		}
	}
	if idToAccept == "" {
		return fmt.Errorf("no job proposal found for job spec %s", spec)
	}
	approvedSpec, err := n.gqlClient.ApproveJobProposalSpec(ctx, idToAccept, false)
	if err != nil {
		return err
	}
	if approvedSpec == nil {
		return fmt.Errorf("no job proposal spec found for job id %s", idToAccept)
	}
	return nil
}

// RegisterNodeToJobDistributor fetches the CSA public key of the node and registers the node with the job distributor
// it sets the node id returned by JobDistributor as a result of registration in the node struct
func (n *Node) RegisterNodeToJobDistributor(ctx context.Context, jd JobDistributor) error {
	// Get the public key of the node
	csaKeyRes, err := n.gqlClient.FetchCSAPublicKey(ctx)
	if err != nil {
		return err
	}
	if csaKeyRes == nil {
		return fmt.Errorf("no csa key found for node %s", n.Name)
	}
	csaKey := strings.TrimPrefix(*csaKeyRes, "csa_")
	// register the node in the job distributor
	registerResponse, err := jd.RegisterNode(ctx, &nodev1.RegisterNodeRequest{
		PublicKey: csaKey,
		Labels:    n.labels,
		Name:      n.Name,
	})

	if err != nil {
		return fmt.Errorf("failed to register node %s: %w", n.Name, err)
	}
	if registerResponse.GetNode().GetId() == "" {
		return fmt.Errorf("no node id returned from job distributor for node %s", n.Name)
	}
	n.NodeId = registerResponse.GetNode().GetId()
	return nil
}

// CreateJobDistributor fetches the keypairs from the job distributor and creates the job distributor in the node
// and returns the job distributor id
func (n *Node) CreateJobDistributor(ctx context.Context, jd JobDistributor) (string, error) {
	// Get the keypairs from the job distributor
	csaKey, err := jd.GetCSAPublicKey(ctx)
	if err != nil {
		return "", err
	}
	// create the job distributor in the node with the csa key
	return n.gqlClient.CreateJobDistributor(ctx, client.JobDistributorInput{
		Name:      "Job Distributor",
		Uri:       jd.WSRPC,
		PublicKey: csaKey,
	})
}

// SetUpAndLinkJobDistributor sets up the job distributor in the node and registers the node with the job distributor
// it sets the job distributor id for node
func (n *Node) SetUpAndLinkJobDistributor(ctx context.Context, jd JobDistributor) error {
	// register the node in the job distributor
	err := n.RegisterNodeToJobDistributor(ctx, jd)
	if err != nil {
		return err
	}
	// now create the job distributor in the node
	id, err := n.CreateJobDistributor(ctx, jd)
	if err != nil {
		return err
	}
	n.JDId = id
	return nil
}

func (n *Node) ExportEVMKeysForChain(chainId string) ([]*clclient.ExportedEVMKey, error) {
	return n.restClient.ExportEVMKeysForChain(chainId)
}

// ReplayLogs replays logs for the chains on the node for given block numbers for each chain
func (n *Node) ReplayLogs(blockByChain map[uint64]uint64) error {
	for sel, block := range blockByChain {
		chainID, err := chainsel.ChainIdFromSelector(sel)
		if err != nil {
			return err
		}
		response, _, err := n.restClient.ReplayLogPollerFromBlock(int64(block), int64(chainID))
		if err != nil {
			return err
		}
		if response.Data.Attributes.Message != "Replay started" {
			return fmt.Errorf("unexpected response message from log poller's replay: %s", response.Data.Attributes.Message)
		}
	}
	return nil
}
