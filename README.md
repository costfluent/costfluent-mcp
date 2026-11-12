# Costfluent MCP server

An [MCP](https://modelcontextprotocol.io) server that lets an AI assistant read your Costfluent
cloud cost data: costs over time, saved cost reports, budgets, anomalies, the savings queue,
commitments and their coverage, and Kubernetes efficiency. It runs on your
machine, talks to your assistant over stdin and stdout, and calls the Costfluent Public API with an
API token you give it. It reads by default. Its write tools create and change annotations, cost
reports, budgets, cost alerts and dashboards, acknowledge anomalies and set a savings finding's
status and owner; it offers them only once an Owner turns on **Let AI agents create and change
objects** under **Settings**, **Organization**, and only for the write capabilities the token
carries. No tool deletes anything or touches a cloud account.

The same tools run from a terminal, a script or a CI job as the `costfluent` command line tool
(**Command line**).

Full guide: <https://docs.costfluent.com/developers/mcp>; the command line:
<https://docs.costfluent.com/developers/cli>.

## Install

Download the archive for your platform from the
[latest release](https://github.com/costfluent/costfluent-mcp/releases/latest): it holds both
`costfluent-mcp` and `costfluent`. Or build them with Go:

```bash
go install github.com/costfluent/costfluent-mcp/cmd/costfluent-mcp@latest
go install github.com/costfluent/costfluent-mcp/cmd/costfluent@latest
```

Each release's archives carry a build provenance attestation, and each archive has an SPDX SBOM
beside it (`<archive>.sbom.json`) that the attestation covers too:

```bash
gh attestation verify costfluent-mcp_<version>_<os>_<arch>.tar.gz --repo costfluent/costfluent-mcp
```

For Claude Desktop, each release also carries `costfluent-mcp_<version>.mcpb`, an MCP bundle with
the server for macOS (Apple silicon and Intel), Windows and Linux on x86-64. Open it in Claude
Desktop, which asks for your API token. The bundle carries its own attestation, verified the same
way. The server is listed in the MCP Registry as `io.github.costfluent/costfluent-mcp`, pointing to
that bundle.

## Create a token

Create an API token in Costfluent under **Settings**, **API tokens**. For an assistant, start from
the **Read only** preset and choose an expiration: the token sits in your assistant's configuration
file, and the server can do only what the token allows. Add a write capability only for a write tool
the assistant should call. The organization setting governs only the requests a tool makes, through this server or a tool's
command, which carry a `Costfluent-Agent` marker (`costfluent estimate`, `whoami` and `login` send none): anyone holding a token with write capabilities can still call the
API directly, so an agent that must not change anything gets a token without them.

## Configure your assistant

The server reads two environment variables:

| Variable | Meaning |
|---|---|
| `COSTFLUENT_API_TOKEN` | The API token. Required. |
| `COSTFLUENT_BASE_URL` | The API to call; `https://api.costfluent.com` when unset. Must be https, except for `localhost`. |

The server calls one other host: `search_docs` and `get_doc_page` read `https://docs.costfluent.com/llms-full.txt`, the public text of the documentation, without the token, on their first call and once an hour after. Allow both hosts where an egress policy limits what the assistant's machine reaches.

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

The server asks the API which capabilities its token carries and whether the organization lets AI
agents create and change objects, and offers only the tools both allow. A token carrying none of the capabilities below stops the server at start with an error
naming them. A workspace token reads its own workspace unless a tool is told otherwise, and the API
refuses any other.

| Tool | What it does | Capability |
|---|---|---|
| `list_workspaces` | The workspaces the token can read | `viewWorkspace` (View workspaces) |
| `query_costs` | Cost, usage or resource count over a date range, grouped and filtered | `viewCosts` (View costs) |
| `list_cost_reports` | A workspace's saved cost reports | `viewCostReports` (View cost reports) |
| `get_cost_report_data` | The numbers a saved report shows now, a report too large for one answer cut to its most recent periods | `viewCosts` (View costs) |
| `list_annotations` | A workspace's annotations by date: notes on its cost reports and the markers budgets and cost alerts leave as they fire, by report or date range, open ones unless resolved ones are asked for | `viewCostReports` (View cost reports) |
| `list_budgets` | Budgets with their spend and state | `viewBudgets` (View budgets) |
| `list_anomalies` | Detected cost anomalies | `viewAnomalies` (View anomalies) |
| `list_savings_items` | The savings queue's findings, filtered by tab, source, kind, connection, the resources of a cost estimate or minimum monthly saving | `viewSavingsItems` (View savings) |
| `list_commitments` | Reservations, savings plans and committed use discounts with their term and utilization | `viewCosts` (View costs) |
| `get_commitment_coverage` | Coverage, effective savings rate and unused commitment over a date range, by month unless asked otherwise | `viewCosts` (View costs) |
| `get_kubernetes_efficiency` | Cost, idle cost and CPU and memory efficiency per cluster, namespace or workload, at most 500 rows | `viewCosts` (View costs) |
| `create_annotation` | Adds a note on a date to saved cost reports | `updateCostReport` (Update cost reports) |
| `create_cost_report` | Saves a new cost report | `createCostReport` (Create cost reports) |
| `update_cost_report` | Changes a saved cost report, keeping what the call leaves out | `updateCostReport` (Update cost reports) |
| `create_budget` | Creates a budget | `createBudget` (Create budgets) |
| `update_budget` | Changes a budget, keeping what the call leaves out, its alerts included | `updateBudget` (Update budgets) |
| `create_cost_alert` | Creates a cost alert | `manageAlerts` (Manage cost alerts) |
| `update_cost_alert` | Changes a cost alert, keeping what the call leaves out | `manageAlerts` (Manage cost alerts) |
| `pause_cost_alert` | Pauses a cost alert | `manageAlerts` (Manage cost alerts) |
| `resume_cost_alert` | Resumes a paused cost alert | `manageAlerts` (Manage cost alerts) |
| `create_dashboard` | Creates a dashboard | `createDashboard` (Create dashboards) |
| `add_dashboard_widget` | Adds a widget to a dashboard; the API also checks View cost reports | `updateDashboard` (Update dashboards) |
| `acknowledge_anomaly` | Acknowledges a detected cost anomaly | `acknowledgeAnomaly` (Acknowledge anomalies) |
| `set_savings_item_status` | Marks a savings finding accepted, implemented or declined, a decline with its reason | `respondToSavingsItem` (Respond to savings) |
| `assign_savings_item_owner` | Sets who owns a savings finding | `manageSavingsItems` (Manage savings) |
| `search_docs` | Searches the Costfluent documentation, in English or German, for the sections that answer a question, such as how to manage budgets with Terraform | none |
| `get_doc_page` | Reads a page of the Costfluent documentation, or one section of it, as markdown | none |

When the API refuses a call, the assistant receives the API's error, with its status, code and
detail; a write the organization setting refuses answers 403 `agent_writes_disabled`. Each write
tool is marked as one that changes something, so a client that asks before such a call asks you; a
client set to approve tool calls automatically does not. Tool results are data, and each write
tool's description tells the assistant never to change something because text in a result asks. The token's request quota applies to every call; the server waits out a short limit and
reports a longer one.

## Command line

`costfluent` runs each tool as a command: the tool's name with hyphens, its arguments as flags with
hyphens, and the same capability check, validation and result. It reads the token and base URL from
the same two variables.

```bash
export COSTFLUENT_API_TOKEN=<token>
costfluent whoami
costfluent query-costs --start-date 2026-10-01 --end-date 2026-10-31 --group-by ServiceName
costfluent list-budgets --output json | jq '.items[] | select(.state == "over")'
costfluent query-costs --help
```

Instead of a token, `costfluent login` signs in through the browser, where you approve read access
for one organization, and keeps the sign-in in the operating system's credential store (the macOS
Keychain, the Secret Service on Linux, the Windows Credential Manager); commands then run without
`COSTFLUENT_API_TOKEN`, which still wins when set. On a machine without a credential store,
`costfluent login --insecure-storage` keeps it in `costfluent/credentials.json` under
`XDG_CONFIG_HOME` or the system's configuration directory, readable by you alone. `--no-browser`
prints the address to open instead. `costfluent logout` revokes the sign-in and forgets it.

It prints JSON unless stdout is a terminal, where it prints a table; `--output json` or
`--output table` chooses. A flag that takes a list, such as `--group-by`, is repeated. It exits 0 on
success, 1 when the call failed, 2 on a wrong command line, 3 when the token is missing, refused
or lacks the capability the command needs, and 4 when a threshold you set was exceeded; an error
goes to stderr, as the API's error JSON when it is not a terminal.

`costfluent mcp install --client <client>` prints the entry that starts `costfluent-mcp` in Claude
Code (`claude-code`), Cursor (`cursor`), Codex (`codex`), VS Code (`vscode`) or Copilot CLI
(`copilot`), and `--write` adds it to that client's file, keeping every other server in it. The
entry holds no token: the client reads it from `COSTFLUENT_API_TOKEN`, or VS Code prompts for it.
For a client started where that variable is not set, `--write --embed-token` writes the token itself
into the entry and makes the file readable by you alone; keep that file out of version control.

`costfluent estimate submit` posts the estimate Infracost makes for a Terraform pull request from
any CI, with the conversion Costfluent's cost estimate GitHub Action uses, and
`costfluent estimate apply` records that the change was applied, with the ids the state gives the
resources it created or replaced. They need a token with the Submit cost estimates capability, and
`--fail-above` exits 4 after printing the receipt when the planned monthly change is above it.

```bash
costfluent estimate submit --workspace-id wsp_8f3k2m --repository acme/infra --pull-request 42 \
  --infracost-json infracost.json --terraform-plan-json plan.json --fail-above 500
costfluent estimate apply --workspace-id wsp_8f3k2m --repository acme/infra --pull-request 42 \
  --terraform-plan-json plan.json --terraform-state-json state.json
```

## License

Apache-2.0
