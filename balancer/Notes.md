# Notes on balancer implementations for xDS

## xDS resolver notes

### About the xDS resources of interest here
- The xDS resolver queries for a `Listener` resource with the resource name
being set to the user's dial target.
- The `Listener` resource contains the name of the `RouteConfiguration`, and it
also contains HTTP filter configuration.
- The `RouteConfiguration` has a list of `VirtualHosts`.
  - Each `VirtualHost` specifies that matching domain that is serves.
  - Upon receiving the `RouteConfiguration` with a list of `VirtualHosts`, the
  most specific matching `VirtualHost` is picked based on the user's dial
  target.
- The `VirtualHost` contains a list of `Routes`.
- `Routes` contain the match conditions and action.
- Action could be to route to a set of weighted clusters.
- Each of the above resources (`VirtualHost`, `Route` and `Cluster`) can specify
overrides for the HTTP filter configuration present in the listener.

### Config Selector
Upon receiving a successful RDS response, the xDS resolver constructs a Config
Selector which is provided the following
- HTTP filter override from the `VirtualHost`.
- Set of `Routes` to match incoming RPCs on and corresponding filter overrides.
- Weighted `Clusters` per route and corresponding filter overrides.

The Config Selector also maintains reference counts for the active RPCs for each
of the clusters. This is shared with the resolver.

### Service Config
xDS resolver generates a service config with the following LB policy configuration
```
message XDSClusterManagerConfig {
    message Child {
      repeated LoadBalancingConfig child_policy = 1;
    }
    map<string, Child> children = 1;
}
```

And as part of the update that is sends to gRPC, it sends the following
- service config
- Config Selector
- xDS client object

## Cluster Manager
This is the top-level LB policy in the xDS LB policy hierarchy. This gets is
configuration through the service config provided by the xDS resolver.

The balancer maintains the following state
- a balancer group
- a state aggregator
- a map from cluster name to child policy configuration
  - see `internal/serviceconfig.go` for child policy configuration parsing

### Config parsing
Config parsing is really simple because the child policy config parsing
functionality is provided by `serviceconfig.LoadBalancingConfig`.

### Config updates from gRPC
Updates from gRPC contains the new LB policy config and also a set of addresses
with a hierarchical path.
- clusters which are not present in the new config are removed from the balancer
group and state aggregator.
- clusters which are new in the new config are added to the balancer group and
state aggregator.
- addresses are split according to their hierarchy path, and the child policy
config and specific addresses are pushed to it.
- if any existing cluster was removed, a new picker to built using the state
aggregator and an update is sent to gRPC. This is similar to what happens when
any child policy pushes a state update.

### Errors from the name resolver
These are pushed to the balancer group which pushed it to all child policies.

### SubConn updates
These are pushed to the balancer group which pushed it to all child policies.

### State updates from child policies
These are handled by the state aggregator's `UpdateState` method, which is
called when any child policy pushes a state update.
- the child policy's aggregated state is computed
  - state changes from `TransientFailure` to `Connecting` are ignored
- overall connectivity state is computed
- a new aggregate picker is constructed

The new picker and connectivity state are pushed to gRPC

### Picker
- We use an aggregate picker which contains the pickers for each of the underlying
clusters (or child policies). 
- The config selector does the route matching based on the incoming RPCs path
and headers, and stores the cluster to which the RPC should be routed in the
context.
- Pickers extracts the cluster from the incoming context.
  - Selects the picker returned by the cluster
  - Delegates the pick to the selected picker

