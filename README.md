![Elastic Etcd](elastic-etcd.png)

# elastic-etcd

An **EXPERIMENTAL** wrapper around [etcd2](https://github.com/coreos/etcd) to add elastic discovery and join.

## Ratio

While etcd was born in the cloud era, it does not really play well in a dynamic environment where nodes come and go and where IP addresses are ephemeral. Moreover, etcd is meant – with its RAFT algorithm at the core – as a consistent key-value store. It rather refuses to form or join a cluster than putting concistency at risk.

As a consequence it is very conservative in implementing advanced cluster member management mechanisms – or even heuristics to make the operation of an etcd cluster more comfortable for the admin.

The elastic-etcd binary experiments with those advanced member management heuristics. It is meant as a frontend to the etcd binary, applying these heuristics and then creating a matching command line for etcd itself.

## Usage

The elastic-etcd binary – in its current incarnation – is called with a subset of the etcd parameters. It does its jobs using the etcd [discovery service](https://coreos.com/os/docs/latest/cluster-discovery.html) and then prints out the matching etcd configuration.

### Output Format

Depending on the context where elastic-etcd is used, it can print out either etcd flags, a systemd dropin or shell environment variables:

- `elastic-etcd -o flags ...` prints `-name=server1 -initial-cluster-state=new...`.

  This allows to embed the elastic-etcd output directly into the command line of etcd using command substituion:
  
  ```bash
  $ export DISCOVERY_URL=$(curl -s 'https://discovery.etcd.io/new?size=3')
  $ set -e
  $ ELASTIC_PARAMS=$(elastic-etcd -v=6 -logtostderr -o flags
      -discovery=$DISCOVERY_URL -name=master2 -client-port=2379 \
      -initial-advertise-peer-urls=http://1.2.3.4:2380 \
    )
  $ etcd2 $ELASTIC_PARAMS \     
       -listen-peer-urls=http://1.2.3.4:2480 \
       -listen-client-urls=http://1.2.3.4:2379 \
       -advertise-client-urls=http://1.2.3.4:2379
   ```

- `elastic-etcd -o dropin ...` prints
   ```
   [Unit]
   After=elastic-etcd.service
   Requires=elastic-etcd.service

   [Service]
   Environment="ETCD_NAME=server1"
   Environment="ETCD_INITIAL_CLUSTER_STATE=new"
   ...
   ```
   
   This allows to call elastic-etcd in the following way, here from the CoreOS cloud-config:
   
    ```yaml
    coreos:
      etcd2:
        name: $private_ipv4
        advertise-client-urls: http://$private_ipv4:2379,http://$private_ipv4:4001
        listen-client-urls: http://$private_ipv4:2379,http://$private_ipv4:4001,http://127.0.0.1:2379,http://127.0.0.1:4001
        listen-peer-urls: http://$private_ipv4:2380
    
      units:
        - name: elastic-etcd.service
          command: start
          content: |
            [Unit]
            Description=Elastic etcd
    
            [Service]
            Type=oneshot
            ExecStartPre=/usr/bin/mkdir -p /var/lib/elastic-etcd
            ExecStartPre=/usr/bin/curl -L -o /var/lib/elastic-etcd/elastic-etcd https://github.com/sttts/elastic-etcd/releases/download/v0.0.7/elastic-etcd
            ExecStartPre=/usr/bin/chmod +x /var/lib/elastic-etcd/elastic-etcd
            ExecStartPre=/usr/bin/mkdir -p /run/systemd/system/etcd2.service.d
            ExecStart=/bin/sh -c "/var/lib/elastic-etcd/elastic-etcd -o dropin -data-dir=/var/lib/etcd2 -initial-advertise-peer-urls=http://$private_ipv4:2380 -name=$private_ipv4 -discovery={{.DiscoveryURL}} -v=6 -logtostderr > /run/systemd/system/etcd2.service.d/99-elastic-etcd.conf"
            ExecStartPost=/usr/bin/systemctl daemon-reload
    ```

- `elastic-etcd -o env ...` prints
   ```bash
   ETCD_NAME=server1
   ETCD_INITIAL_CLUSTER_STATE=new
   ...
   ```
   
   This allows to call elastic-etcd in the following way:
   ```bash
   $ eval $(elastic-etcd -v=6 -logtostderr -discovery=$DISCOVERY_URL -o flags \
                   -name=master2 -client-port=2379 \
                   -initial-advertise-peer-urls=http://1.2.3.4:2380 \
      )
   $ etcd2 -listen-peer-urls=http://1.2.3.4:2480 \
           -listen-client-urls=http://1.2.3.4:2379 \
           -advertise-client-urls=http://1.2.3.4:2379
   ```

### Command Line Help

```
NAME:
   elastic-etcd - auto join a cluster, either during bootstrapping or later

USAGE:
   elastic-etcd [global options] command [command options] [arguments...]

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   -o "env"                   the output format out of: env, dropin, flags
   --join-strategy "replace"  the strategy to join: dumb, replace, add
                              [$ELASTIC_ETCD_JOIN_STRATEGY]
   --client-port "2379"       the etcd client port of all peers [$ELASTIC_ETCD_CLIENT_PORT]
   --cluster-size "-1"        the maximum etcd cluster size, default: size value of
                              discovery url, 0 for infinit [$ELASTIC_ETCD_CLUSTER_SIZE]

   --discovery                a etcd discovery url [$ELASTIC_ETCD_DISCOVERY]
   --data-dir                 the etcd data directory [$ELASTIC_ETCD_DATA_DIR]
   --name                     the cluster-unique node name [$ELASTIC_ETCD_NAME]
   --initial-advertise-peer-urls "http://localhost:2380"  the advertised peer urls
                              of this instance [$ELASTIC_ETCD_INITIAL_ADVERTISE_PEER_URLS]

   --alsologtostderr=false    log to standard error as well as files
   --log_backtrace_at=:0      when logging hits line file:N, emit a stack trace
   --log_dir=                 If non-empty, write log files in this directory
   --logtostderr=false        log to standard error instead of files
   --stderrthreshold=2        logs at or above this threshold go to stderr
   --v=0                      log level for V logs
   --vmodule=                 comma-separated list of pattern=N settings for
                              file-filtered logging
   --help, -h                 show help
```

The first block of flags is used to control the elastic-etcd algorithm:
- `-o`: compare [above](#output-format)
- `--join-strategy`: can be one of **prepared**, **replace**, **prune**, **add**:
  - **prepare**: assumes that the admin prepares new member entries
  - **add**: adds a member until the cluster is full, never removes old members
  - **replace** (default): defensively removes a dead member, i.e. only when a cluster is full. Then adds itself.
  - **prune**: aggressively removes all dead members. Then adds itself.
- `--client-port`: for health checking using the entries in the discovery service url this port is used. At the discovery time there is no client url known, only peer urls. In order to get the current cluster state, a client url is necessary though. This of course only works if all client urls of the cluster members use the same port.
- `--cluster-size`: by default the discovery url cluster size is used to limit addition of new members. Using `--cluster-size` this can be overridden, e.g. to grow a cluster after bootstrapping.

The second block of flags has the same meaning as for etcd. Though, the elastic-etcd algorithm might decide to change the values of those flags and pass them to etcd (via one of the output modes).

## How To Build

```bash
$ export GOPATH=$PWD
$ mkdir -p pkg src bin
$ go get github.com/sttts/elastic-etcd
$ cd src/github.com/sttts/elastic-etcd
$ make build
$ ./elastic-etcd --help
```

## Join Strategies

For experimentation the elastic-etcd algorithm supports a number of join strategies (compare flag description above). In the following these are discussed:

The **prepare** strategy resembles the default behavior of etcd. In this mode, the admin has to remove old member and add a new entry *before* the new etcd instance actually starts up.

The **add** strategy is also similar to the default behavior of etcd, but adds the capability for a new etcd instance to join an existing cluster, as long as the cluster is below the given `--cluster-size` size (defaulting to the discovery url cluster size).

The **replace** strategy is probably the right mix between old conservative behavior and the needs of a dynamic cloud environment where IPs come and go when machines are replaced. It will behave like the **add** behavior, but in addition it will health check all cluster member and eventually remove one dead member before adding itself. This removal though is only done when the cluster would be full otherwise. I.e. during cluster growth (e.g. when the user passes `--cluster-size` long after bootstrapping) this strategy behaves conservatively without removal of any member.

Finally the **prune** strategy is like **replace**, but it will always remove every dead member before adding the new instance.

In all of the last three strategies a quorum calculation is done to protect the cluster from putting the quorum at risk when a new instance joins: *If a quorum is put at risk when a new instance fails to startup, the whole join process is stopped before even trying to join*.

## Credits

This work is inspired by

- Johannes Ziemke's [CloudFormation driven Consul in AutoScalingGroup](https://5pi.de/2015/04/27/cloudformation-driven-consul-in-autoscalinggroup/)
- T.J. Corrigan's [etcd Clustering in AWS –
Configuring a robust etcd cluster in an AWS Auto Scaling Group](http://engineering.monsanto.com/2015/06/12/etcd-clustering/).
