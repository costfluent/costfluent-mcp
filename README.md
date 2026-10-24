# Costfluent MCP server

An [MCP](https://modelcontextprotocol.io) server that lets an AI assistant read your Costfluent
cloud cost data: costs over time, saved cost reports, budgets and anomalies. It runs on your
machine, talks to your assistant over stdin and stdout, and calls the Costfluent Public API with an
API token you give it. It reads; it never changes anything.

Full guide: <https://docs.costfluent.com/developers/mcp>.

## Install

Download the archive for your platform from the
[latest release](https://github.com/costfluent/costfluent-mcp/releases/latest), or build it with Go:

```bash
go install github.com/costfluent/costfluent-mcp/cmd/costfluent-mcp@latest
```

Each release's archives carry a build provenance attestation, and each archive has an SPDX SBOM
beside it (`<archive>.sbom.json`) that the attestation covers too:

```bash
gh attestation verify costfluent-mcp_<version>_<os>_<arch>.tar.gz --repo costfluent/costfluent-mcp
```

## Create a token

Create an API token in Costfluent under **Settings**, **API tokens**. For an assistant, start from
the **Read only** preset and choose an expiration: the token sits in your assistant's configuration
file, and the server can do only what the token allows.

## Configure your assistant

The server reads two environment variables:

| Variable | Meaning |
|---|---|
| `COSTFLUENT_API_TOKEN` | The API token. Required. |
| `COSTFLUENT_BASE_URL` | The API to call; `https://api.costfluent.com` when unset. Must be https, except for `localhost`. |

Claude Code:

```bash
claude mcp add costfluent --env COSTFLUENT_API_TOKEN=<token> -- costfluent-mcp
```

Claude Desktop, Cursor and other clients that read an `mcpServers` file:

```json
{
  "mcpServers": {
    "costfluent": {
      "command": "costfluent-mcp",
      "env": { "COSTFLUENT_API_TOKEN": "<token>" }
    }
  }
}
```

## Tools

The server asks the API which capabilities its token carries and offers only the tools the token
can call. A token carrying none of the capabilities below stops the server at start with an error
naming them. A workspace token reads its own workspace unless a tool is told otherwise, and the API
refuses any other.

| Tool | What it reads | Capability |
|---|---|---|
| `list_workspaces` | The workspaces the token can read | `viewWorkspace` (View workspaces) |
| `query_costs` | Cost, usage or resource count over a date range, grouped and filtered | `viewCosts` (View costs) |
| `list_cost_reports` | A workspace's saved cost reports | `viewCostReports` (View cost reports) |
| `get_cost_report_data` | The numbers a saved report shows now | `viewCosts` (View costs) |
| `list_budgets` | Budgets with their spend and state | `viewBudgets` (View budgets) |
| `list_anomalies` | Detected cost anomalies | `viewAnomalies` (View anomalies) |

When the API refuses a call, the assistant receives the API's error, with its status, code and
detail. The token's request quota applies to every call; the server waits out a short limit and
reports a longer one.

## License

Apache-2.0
