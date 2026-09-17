# OpAMP Example Server

An example OpAMP Server with an admin UI, a metrics exporter and an S3 backed
config store.

```shell
make build-example-server   # from the repository root
server/bin/server           # from internal/examples
```

Without `-s3-bucket` the Server keeps the configs in memory only, which is enough
to click through the UI but loses every config on restart. Point it at a bucket
to make the configs durable.

Or run the whole thing — Server, Agent and a MinIO bucket — with
`make docker-compose-up` from `internal/examples`, see
[Trying it out](#trying-it-out).

The OpAMP endpoint listens on `:4320` (`wss://<host>:4320/v1/opamp`), the admin
UI on <http://localhost:4321>.

## Config store

The configs the Server hands out to the Agents are plain OpenTelemetry Collector
config files kept in an S3 bucket. **The bucket is the source of truth**: the
Server holds no authoritative config of its own, it only distributes what the
bucket contains.

That means:

- A config edited in the admin UI is written to S3 first, and is only offered to
  the Agents once S3 accepted the write. If the write fails, nothing is sent to
  any Agent.
- A config changed directly in the bucket (by a pipeline, a GitOps job, or
  `aws s3 cp`) is picked up by the periodic sync and pushed to every Agent it
  applies to.
- Restarting or scaling out the Server does not change any Agent's config,
  because the Server rebuilds its view from the bucket on startup.

### Bucket layout

Agents are addressed by the **component** they run and the **stack** they run in,
the two attributes they report in their `AgentDescription`. Individual instances
and hosts are deliberately not addressable: every instance of a component in a
stack runs the same config.

Config files are stored verbatim, so they can be read and edited with any tool:

```
<bucket>/<prefix>/<component>/<stack>/config.yaml   config for a component in one stack
<bucket>/<prefix>/<component>/config.yaml           config for a component in every stack
<bucket>/<prefix>/config.yaml                       fallback config for every other Agent
```

The prefix defaults to `otel-collector`, so with `-s3-bucket my-bucket` an agent
reporting `component=billing` and `stack=prod` is configured from
`s3://my-bucket/otel-collector/billing/prod/config.yaml`.

An Agent gets the config from the first of those folders that holds one, so a
component/stack file overrides a component wide file, which overrides the
fallback. Attribute characters that are not `[A-Za-z0-9._-]` are replaced with
`_` when they are turned into a key segment.

Exactly one config file per folder is expected. The Server always writes
`config.yaml`, but a file a pipeline put there under a different name is picked
up just as well; if a folder holds several config files, the first one in
lexicographic order wins.

Objects under the prefix that do not end in `.yaml` or `.yml` are ignored, so the
bucket can also hold READMEs, checksums or other bookkeeping files.

Enabling **bucket versioning** is recommended: it gives a full history of every
config change and turns a rollback into a bucket operation.

### Configuration

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-s3-bucket` | `OPAMP_S3_BUCKET` | _(empty)_ | Bucket holding the config files. When empty, configs are kept in memory only and are lost on restart. |
| `-s3-prefix` | `OPAMP_S3_PREFIX` | `otel-collector` | Key inside the bucket the config files live under. An empty value falls back to the default. |
| `-s3-region` | `OPAMP_S3_REGION` | _(from AWS config)_ | Region of the bucket. |
| `-s3-endpoint` | `OPAMP_S3_ENDPOINT` | _(AWS)_ | Endpoint override for S3 compatible stores (MinIO, LocalStack). |
| `-s3-path-style` | `OPAMP_S3_PATH_STYLE` | `false` | Use `<endpoint>/<bucket>` addressing. Required by most S3 compatible stores. |
| `-s3-kms-key-id` | `OPAMP_S3_KMS_KEY_ID` | _(bucket default)_ | KMS key to encrypt config files written by the Server with. |
| `-s3-sync-interval` | `OPAMP_S3_SYNC_INTERVAL` | `30s` | How often the bucket is polled for changes made outside of the Server. |

Credentials are taken from the default AWS credential chain (environment,
shared config, IRSA/instance role, ...).

The Server refuses to start if a bucket is configured but cannot be read, so a
misconfigured bucket or a missing permission is reported at startup instead of
silently handing out empty configs.

### Agent attributes

The Server reads two attributes from the Agent description, either of which may
be identifying or non-identifying:

| Attribute | Used for |
| --- | --- |
| `component` | The `<component>` folder the config is looked up in. |
| `stack` | The `<stack>` folder inside the component folder. |

An Agent that reports neither gets the fallback `config.yaml`; one that reports
only a component gets the component wide file. The example Agent sends both when
started with `-component`/`-stack` (`AGENT_COMPONENT`/`AGENT_STACK`).

### IAM permissions

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::my-bucket",
      "Condition": { "StringLike": { "s3:prefix": "otel-collector/*" } }
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject"],
      "Resource": "arn:aws:s3:::my-bucket/otel-collector/*"
    }
  ]
}
```

`s3:PutObject` is only needed if configs are edited through the admin UI. A
Server that is fed by a pipeline can run read-only, in which case the UI save
button fails with a permission error and nothing is sent to the Agents.

### Failure behavior

The Server is biased towards keeping Agents running on the config they already
have:

- A failed sync keeps the last known configs and is retried on the next tick.
- A config file that disappears from the bucket does **not** wipe the Agent's
  config; the Agent keeps running what it was last given.
- A failed write leaves every Agent untouched and is reported in the UI.

### Trying it out

The docker compose stack in `internal/examples` runs the Server, an Agent and a
MinIO bucket wired together, which is enough to see the whole round trip:

```shell
make docker-compose-up      # from internal/examples
make docker-compose-logs
```

| Service | Address | Credentials |
| --- | --- | --- |
| Admin UI | <http://localhost:4321> | |
| MinIO console | <http://localhost:9001> | `minioadmin` / `minioadmin` |
| MinIO API | <http://localhost:9000> | |

The `minio-init` service creates the bucket and uploads
[`server/configs/config.yaml`](configs/config.yaml) to
`opamp-configs/otel-collector/config.yaml` on first start. The bucket lives in a
named volume, so later edits survive `make docker-compose-down` and are not
overwritten by the seed (`docker compose down -v` resets it).

The example Agent reports `component=example-collector` and `stack=dev` in the
stack (`AGENT_COMPONENT`/`AGENT_STACK`), so it starts on the fallback config and
its own config file would be
`otel-collector/example-collector/dev/config.yaml`.

Two ways to watch the sync work, with `OPAMP_S3_SYNC_INTERVAL` set to 5s in the
stack:

```shell
# 1. Change the config in the bucket and watch it reach the agent.
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin AWS_REGION=us-east-1
aws --endpoint-url http://localhost:9000 \
  s3 cp my-config.yaml s3://opamp-configs/otel-collector/config.yaml

# 2. Change the config in the admin UI and watch it appear in the bucket.
aws --endpoint-url http://localhost:9000 \
  s3 cp s3://opamp-configs/otel-collector/config.yaml -
```

The agent page in the UI shows the effective config the Agent reports back, so
both paths are visible end to end.

Uploading a narrower file takes precedence over the fallback:

```shell
# Applies to every example agent in the dev stack.
aws --endpoint-url http://localhost:9000 s3 cp my-config.yaml \
  s3://opamp-configs/otel-collector/example-collector/dev/config.yaml

# Applies to the example agents in every stack.
aws --endpoint-url http://localhost:9000 s3 cp my-config.yaml \
  s3://opamp-configs/otel-collector/example-collector/config.yaml
```

Scale the Agents to see a config file fan out to every instance of the component
in the stack:

```shell
make docker-compose-scale AGENTS=3
```

### Pointing the stack at a real bucket

Every setting is passed through from the environment, so an `.env` file in
`internal/examples` (or exported variables) is enough to run the same Server and
Agent against AWS. Setting `OPAMP_S3_ENDPOINT` to an empty value switches the
Server from MinIO to AWS:

```shell
OPAMP_S3_BUCKET=my-bucket
OPAMP_S3_PREFIX=otel-collector
OPAMP_S3_ENDPOINT=            # explicitly empty: talk to AWS, not to MinIO
OPAMP_S3_PATH_STYLE=false
AWS_REGION=eu-central-1
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
```

Start it without the local bucket:

```shell
docker compose up --build --no-deps opamp-server opamp-agent
```

### Running the Server against MinIO without docker compose

```shell
# 1. Start an S3 compatible store.
docker run -d --name minio -p 9000:9000 -p 9001:9001 \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  minio/minio server /data --console-address ":9001"

# 2. Create the bucket and a config file.
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin AWS_REGION=us-east-1
aws --endpoint-url http://localhost:9000 s3 mb s3://opamp-configs
aws --endpoint-url http://localhost:9000 \
  s3 cp server/configs/config.yaml s3://opamp-configs/otel-collector/config.yaml

# 3. Run the Server against it.
server/bin/server \
  -s3-bucket opamp-configs \
  -s3-endpoint http://localhost:9000 \
  -s3-path-style \
  -s3-sync-interval 5s

# 4. Run an agent that asks for the config of a component in a stack.
agent/bin/agent -component example-collector -stack dev \
  -endpoint wss://127.0.0.1:4320/v1/opamp -tls-insecure_skip_verify
```

## Implementation

| Package | Responsibility |
| --- | --- |
| `configstore` | Backend abstraction (`S3Backend`, `MemoryBackend`), the cached and periodically synced `Store`, and the key layout. |
| `data` | Agent state. `Agents.SaveConfigForAgent` writes through the store, `Agents.ReloadConfigsFromStore` applies changes the store picked up. |
| `opampsrv` | The OpAMP endpoint. |
| `uisrv` | The admin UI. |

Reads are served from the store's in-memory cache, so an Agent's status update
never blocks on an S3 round trip. Writes always go to S3 first.
