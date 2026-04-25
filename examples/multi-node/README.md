# Multi-node deployment example

A 3-node MeshCDN cluster spanning two regions, with shared SSL certs and a shared rule template library.

## Topology

```
                        ┌─────────────────┐
                        │ Telegram group  │
                        └────────┬────────┘
                                 │
                   ┌─────────────┴───────────┐
                   ▼                         ▼
        ┌─────────────────┐         ┌─────────────────┐
        │ Node A          │ ◄─────► │ Node B          │
        │ Frankfurt       │  9443   │ New York        │
        │ 192.0.2.10      │         │ 192.0.2.20      │
        │ (Bot node)      │         │                 │
        └────────┬────────┘         └────────┬────────┘
                 │                           │
                 ▼                           ▼
              users                       users

                          ┌─────────────────┐
                          │ Node C          │
                          │ Singapore       │
                          │ 192.0.2.30      │
                          └─────────────────┘
                                  ▲
                                  │
                                users
```

DNS:
```
example.com → 192.0.2.10
example.com → 192.0.2.20
example.com → 192.0.2.30
```

Browsers / resolvers will pick one of the three IPs and retry on failure.

## Step 1: Set up Node A (the introducer)

This is the first node, also the initial Bot node.

```bash
# On Node A (192.0.2.10)
ssh root@192.0.2.10
wget https://github.com/<org>/meshcdn/releases/latest/download/meshcdn-linux-amd64.tar.gz
tar xzf meshcdn-linux-amd64.tar.gz

sudo bash install.sh \
  --bot-token="<your_bot_token>" \
  --group-id="<your_group_id>"
```

Verify:
```bash
sudo systemctl status meshcdn
```

In Telegram:
```
/v nodes
```

Should show 1 node, status online.

## Step 2: Add Node B

```bash
# On Node B (192.0.2.20)
ssh root@192.0.2.20
curl -s http://192.0.2.10:9443/mesh/bootstrap | sudo bash -s -- \
  192.0.2.10 \
  "<your_bot_token>" \
  <your_group_id>
```

This downloads the binary from Node A and provisions Node B. Watch the install:
```bash
sudo journalctl -u meshcdn -n 30 --no-pager
```

You should see:
```
开始认亲: 192.0.2.10
认亲成功! JoinOrder=2, 节点数=2
同步配置: <count> 条命令
```

In Telegram:
```
/v nodes
```

Now shows 2 nodes.

## Step 3: Add Node C

```bash
# On Node C (192.0.2.30) — note we can use *any* existing peer as introducer
ssh root@192.0.2.30
curl -s http://192.0.2.20:9443/mesh/bootstrap | sudo bash -s -- \
  192.0.2.20 \
  "<your_bot_token>" \
  <your_group_id>
```

In Telegram:
```
/v nodes
```

Now shows 3 nodes.

## Step 4: Add domain (one command, syncs to all 3 nodes)

```
/w domain https://example.com 443 https://1.2.3.4:443
```

The command broadcasts to all peers. Verify:

```
/v domain example.com
```

Each node now has the same domain configured.

## Step 5: Issue SSL certificate

```
/w ssl example.com
```

The Bot node (Node A) does the ACME validation and issues the cert, then broadcasts the cert PEM to Nodes B and C. They each write the cert files locally.

Verify on every node:
```bash
sudo ls -la /etc/meshcdn/certs/example.com.crt
```

Or in Telegram:
```
/v ssl health
```

Should show `example.com` as `live` with the same fingerprint everywhere.

## Step 6: Add DNS records

In your DNS provider, point your domain at all three node IPs:

```
example.com.   60   IN   A   192.0.2.10
example.com.   60   IN   A   192.0.2.20
example.com.   60   IN   A   192.0.2.30
```

Wait for propagation (typically 1-5 minutes with TTL 60).

## Step 7: Test failover

```bash
# Test from your local machine
for i in 1 2 3 4 5; do
  curl -s -I https://example.com 2>&1 | head -1
done
```

Each request might hit a different node depending on which IP your resolver returns.

To test failure handling:
```bash
# Stop one node temporarily
ssh root@192.0.2.20 'sudo systemctl stop meshcdn'

# Continue testing — should still work via the other two
curl -I https://example.com

# Restart it
ssh root@192.0.2.20 'sudo systemctl start meshcdn'
```

## Step 8: Add a rule template (cluster-wide)

Define a rule template once, apply to many domains:

```
/w ruleset img-cache cache *.jpg,*.png,*.gif,*.webp 604800
/w ruleset sec-headers header
response add X-Frame-Options DENY
response add Strict-Transport-Security "max-age=31536000"
response del Server

/w domain example.com #+img-cache
/w domain example.com #+sec-headers
```

These template references propagate to all 3 nodes. Verify on each:
```
/v domain example.com
```

Each node should show the same template references.

## Common operations

### Check cluster health

```
/v nodes        # peer status
/v ssl health   # cert status across cluster
/v stats        # access traffic
```

### Move the Bot role to a different node

```
/target 192.0.2.20
```

Useful if Node A is going down for maintenance.

### Cluster-wide upgrade

```
/upgrade
```

The Bot node uploads its `cdn-agent` binary to all peers and restarts them in sequence.

### Backup before upgrade (automatic since v3.1.0-alpha8)

```bash
sudo cdn-agent restore --list  # see backups
```

If something goes wrong:
```bash
sudo systemctl stop meshcdn
sudo cdn-agent restore --backup=<timestamp>
sudo systemctl start meshcdn
```

## Tips

- **TTL on DNS records**: keep TTL low (60-300s) so you can quickly remove a dead node from rotation. (Automatic DNS removal is on the roadmap.)
- **Geographic placement**: pick regions close to your users. Latency between users and the nearest node is what matters most.
- **Start with 3 nodes minimum**: 2 is fragile (any one failure = single point of failure). 3+ tolerates one node down without losing redundancy.
- **Same OS on all nodes**: simplifies operations. Ubuntu 22.04 LTS is the recommended baseline.
- **Don't share VPS providers**: if all 3 nodes are on the same provider, a provider-wide outage takes you down. Spread across 2+ providers.

## Cost

Roughly:
- 3 VPS instances at $5-10/month each = $15-30/month
- Domain ~ $10/year
- Total: $15-30/month for a redundant 3-node CDN

Compare to commercial CDN starting tiers (typically $20-200/month for similar redundancy and you'd still be using their infrastructure).
