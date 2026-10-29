# Console screenshots

The README images show the console's actual HTML/CSS/JavaScript, served with
fictional data. They contain no deployment configuration, credentials, tenant
inventory or production telemetry.

From the framework repository:

```bash
python3 docs/demo/serve.py
```

Open <http://127.0.0.1:8765> in a browser. Capture **Overview**, then choose **Tenants** and filter to **atlas**, saving JPEGs in `docs/images/console-overview.jpg` and
`docs/images/console-tenants.jpg`. Use the browser's normal viewport and screenshot facility.
The shell includes a visible `demo` label. Current timestamps are generated at
server startup, so restart before capturing a fresh set.

Only Overview and Tenants are populated. Other views explicitly report that
they are not set up in this preview. These fixtures illustrate the interface;
they do not verify upstream integrations or prove health of a live cluster.
Runtime integration suites under `docker/infra-console/integration` retain that
separate responsibility.
