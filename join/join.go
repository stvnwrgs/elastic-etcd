package join

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/client"
	"github.com/coreos/etcd/rafthttp"
	"github.com/golang/glog"
	"github.com/sttts/elastic-etcd/discovery"
	"golang.org/x/net/context"
	"golang.org/x/net/context/ctxhttp"
)

// Strategy describes the member add strategy.
type Strategy string

const (
	livenessTimeout = time.Second * 10
	etcdTimeout     = time.Second * 30

	// PreparedStrategy assumes that the admin prepares new member entries.
	PreparedStrategy = Strategy("prepared")

	// PruneStrategy aggressively removes dead members.
	PruneStrategy = Strategy("prune")

	// ReplaceStrategy defensively removes a dead member only when a cluster is full.
	ReplaceStrategy = Strategy("replace")

	// AddStrategy only adds a member until the cluster is full, never removes old members.
	AddStrategy = Strategy("add")

	maxUint = ^uint(0)
	maxInt  = int(maxUint >> 1)
)

// EtcdConfig is the result of the join algorithm, turned into etcd flags or env vars.
type EtcdConfig struct {
	InitialCluster      []string
	InitialClusterState string
	AdvertisePeerURLs   string
	Discovery           string
	Name                string
}

func alive(ctx context.Context, m client.Member) bool {
	ctx, _ = context.WithTimeout(ctx, livenessTimeout)
	glog.V(6).Infof("Testing liveness of %s=%v", m.Name, m.PeerURLs)
	for _, u := range m.PeerURLs {
		resp, err := ctxhttp.Get(ctx, http.DefaultClient, u+rafthttp.ProbingPrefix)
		if err == nil && resp.StatusCode == http.StatusOK {
			return true
		}
	}

	return false
}

func active(ctx context.Context, m client.Member) (bool, error) {
	ctx, _ = context.WithTimeout(ctx, etcdTimeout)

	c, err := client.New(client.Config{
		Endpoints:               m.ClientURLs,
		Transport:               client.DefaultTransport,
		HeaderTimeoutPerRequest: 5 * time.Second,
	})
	if err != nil {
		return false, err
	}
	mapi := client.NewMembersAPI(c)
	glog.V(6).Infof("Testing whether %s=%v knows the leader", m.Name, m.PeerURLs)
	leader, err := mapi.Leader(ctx)
	if err != nil {
		return false, err
	}
	return leader != nil, nil
}

func clusterExistingHeuristic(
	ctx context.Context,
	size int, nodes []discovery.Machine,
) ([]discovery.Machine, error) {
	quorum := size/2 + 1

	if nodes == nil {
		glog.V(4).Infof("No nodes found in discovery service. Assuming new cluster.")
		return nil, nil
	}

	wg := sync.WaitGroup{}
	wg.Add(len(nodes))
	lock := sync.Mutex{}
	activeNodes := make([]discovery.Machine, 0, len(nodes))
	for _, n := range nodes {
		go func(n discovery.Machine) {
			defer wg.Done()
			if !alive(ctx, n.Member) {
				glog.Infof("Node %s looks dead", n.NamedPeerURLs())
				return
			}
			if ok, err := active(ctx, n.Member); !ok {
				if err != nil {
					glog.Error(err)
				}
				glog.Infof("Node %s is not in a healthy cluster.", n.NamedPeerURLs())
				return
			}
			glog.Infof("Node %s looks alive and active in a cluster", n.NamedPeerURLs())
			lock.Lock()
			defer lock.Unlock()
			activeNodes = append(activeNodes, n)
		}(n)
	}
	wg.Wait()

	if len(nodes) < quorum {
		glog.V(4).Infof(
			"Only %d nodes found in discovery service, less than a quorum of %d. Assuming new cluster.",
			len(nodes),
			quorum,
		)
		return nil, nil
	}

	if len(nodes) == size {
		glog.V(4).Infof("Cluster is full. Assuming existing cluster.")
		return activeNodes, nil
	}

	if len(activeNodes) > 0 {
		return activeNodes, nil
	}

	return nil, nil
}

// Join adds a new member depending on the strategy and returns a matching etcd configuration.
func Join(
	discoveryURL, name, initialAdvertisePeerURLs string,
	fresh bool,
	clientPort, clusterSize int,
	strategy Strategy,
) (*EtcdConfig, error) {
	ctx := context.Background()

	res, err := discovery.Value(ctx, discoveryURL, "/")
	if err != nil {
		return nil, err
	}
	nodes := make([]discovery.Machine, 0, len(res.Node.Nodes))
	for _, nn := range res.Node.Nodes {
		if nn.Value == nil {
			glog.V(5).Infof("Skipping %q because no value exists", nn.Key)
		}
		var n *discovery.Machine
		n, err = discovery.NewDiscoveryNode(*nn.Value, clientPort)
		if err != nil {
			glog.Warningf("invalid peer url %q in discovery service: %v", *nn.Value, err)
			continue
		}
		nodes = append(nodes, *n)
	}

	if clusterSize < 0 {
		res, err = discovery.Value(ctx, discoveryURL, "/_config/size")
		if err != nil {
			return nil, fmt.Errorf("cannot get discovery url cluster size: %v", err)
		}

		size, _ := strconv.ParseInt(*res.Node.Value, 10, 16)
		clusterSize = int(size)

		glog.V(2).Infof("Got a target cluster size of %d from the discovery url", clusterSize)
	} else if clusterSize == 0 {
		clusterSize = maxInt
	}

	activeNodes, err := clusterExistingHeuristic(ctx, clusterSize, nodes)
	if err != nil {
		return nil, err
	}

	if activeNodes != nil && len(activeNodes) == 0 {
		// cluster down. Restarting nodes with the same config.
		if fresh {
			return nil, errors.New("Cluster is down. A new node cannot join now.")
		}

		glog.Infof("Existing cluster seems to be done. No healthy node found. Trying to resume cluster.")

		return &EtcdConfig{
			InitialClusterState: "existing",
			AdvertisePeerURLs:   initialAdvertisePeerURLs,
			Name:                name,
		}, nil
	} else if activeNodes != nil {
		activeNamedURLs := make([]string, 0, len(nodes))
		for _, n := range activeNodes {
			activeNamedURLs = append(activeNamedURLs, n.NamedPeerURLs()...)
		}

		advertisedURLs := strings.Split(initialAdvertisePeerURLs, ",")

		advertisedNamedURLs := make([]string, 0, len(initialAdvertisePeerURLs))
		for _, u := range advertisedURLs {
			advertisedNamedURLs = append(advertisedNamedURLs, fmt.Sprintf("%s=%s", name, u))
		}

		initialNamedURLs := []string{advertisedNamedURLs[0]}
		if strategy != PreparedStrategy && fresh {
			glog.Infof("Existing cluster found. Trying to join with %q strategy.", string(strategy))

			adder, err := newMemberAdder(
				activeNodes,
				strategy,
				clientPort,
				clusterSize,
				discoveryURL,
			)
			if err != nil {
				return nil, err
			}
			initialURLs, err := adder.Add(ctx, name, advertisedURLs)
			if err != nil {
				return nil, fmt.Errorf("unable to add node %q with peer urls %q to the cluster: %v", name, initialAdvertisePeerURLs, err)
			}

			initialNamedURLs = []string{}
			for _, u := range initialURLs {
				initialNamedURLs = append(initialNamedURLs, fmt.Sprintf("%s=%s", name, u))
			}
		} else {
			glog.Infof("Existing cluster found. Trying to join without adding this instance as a member.")
		}

		return &EtcdConfig{
			InitialCluster:      append(initialNamedURLs, activeNamedURLs...),
			InitialClusterState: "existing",
			AdvertisePeerURLs:   initialAdvertisePeerURLs,
			Name:                name,
		}, nil
	} else {
		glog.Infof("Trying to launch new cluster.")

		return &EtcdConfig{
			InitialClusterState: "new",
			Discovery:           discoveryURL,
			AdvertisePeerURLs:   initialAdvertisePeerURLs,
			Name:                name,
		}, nil
	}
}
