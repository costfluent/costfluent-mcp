package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/estimate"
	"github.com/costfluent/costfluent-mcp/internal/server"
)

const estimateHelp = `Usage: costfluent estimate submit --workspace-id <id> --repository <owner/name> --pull-request <number>
         --infracost-json <file> [--terraform-plan-json <file>] [--commit <sha>] [--url <https URL>]
         [--infracost-version <version>] [--fail-above <amount>] [--output json|table]
       costfluent estimate apply --workspace-id <id> --repository <owner/name> --pull-request <number>
         --terraform-plan-json <file> --terraform-state-json <file> [--output json|table]

Posts a pull request's cost estimate to Costfluent from any CI, as the cost estimate Action does in
GitHub Actions and with the Action's own conversion, so both post the same estimate.

submit converts Infracost's JSON (Infracost v0.10's infracost diff --format json, or Infracost v2's
infracost diff --json) into the estimate and posts it, which creates the pull request's estimate or
replaces it until the change is applied. With --terraform-plan-json, terraform show -json of the
saved plan, each changed resource carries the ids Costfluent measures its cost by. A diff with no
change to a priced resource posts nothing. It prints the estimate's receipt: its id, its state and
the planned monthly totals Costfluent computed.

apply posts the ids the state after the apply gives every resource the plan created or replaced,
which records the pull request's estimate as applied. A resource the state holds no id for is
named in a warning and left out; a pull request with no estimate in the workspace has nothing to
apply, which is no failure.

COSTFLUENT_API_TOKEN holds an API token with the Submit cost estimates capability; no flag takes it.

Flags:
  --workspace-id <id>            required: the workspace the estimate belongs to
  --repository <owner/name>      required: the repository, such as acme/infra
  --pull-request <number>        required: the pull request number
  --infracost-json <file>        submit, required: Infracost's JSON
  --terraform-plan-json <file>   terraform show -json of the saved plan: optional for submit,
                                 required for apply, where it is the plan that was applied
  --terraform-state-json <file>  apply, required: terraform show -json of the state after the apply
  --commit <sha>                 submit: the commit the estimate is for
  --url <https URL>              submit: a link to the pull request
  --infracost-version <version>  submit: the Infracost version that wrote the JSON
  --fail-above <amount>          submit: exit 4, after printing the receipt, when the planned
                                 monthly change is above this amount in the estimate's currency;
                                 an equal change passes and a negative one never fails
  --output <json|table>          JSON unless stdout is a terminal

Exit status: 0 posted, or nothing to post or apply; 1 the conversion or the request failed; 2 a
wrong command line or an input file that cannot be read as JSON; 3 no access; 4 the planned monthly
change is above --fail-above.
`

const (
	estimateSubmit = "submit"
	estimateApply  = "apply"
)

// The flags the commands require, and those the input files come from.
const (
	flagWorkspaceID = "workspace-id"
	flagRepository  = "repository"
	flagPullRequest = "pull-request"
	flagInfracost   = "infracost-json"
	flagPlan        = "terraform-plan-json"
	flagState       = "terraform-state-json"
)

// maxAppliedResources is the most resources one apply takes, the contract's limit.
const maxAppliedResources = 2000

var (
	pullRequestPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	// A threshold is non-negative: a negative one would fail every pull request that saves nothing.
	failAbovePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

// estimateOptions is an estimate submit or apply command line.
type estimateOptions struct {
	workspaceID, repository, pullRequest string
	infracost, plan, state               string
	commit, url, infracostVersion        string
	failAbove                            string
	output                               outputFormat
}

func parseEstimate(sub string, argv []string, output outputFormat) (estimateOptions, error) {
	o := estimateOptions{output: output}
	fs := flag.NewFlagSet("estimate "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.workspaceID, flagWorkspaceID, "", "")
	fs.StringVar(&o.repository, flagRepository, "", "")
	fs.StringVar(&o.pullRequest, flagPullRequest, "", "")
	fs.StringVar(&o.plan, flagPlan, "", "")
	fs.Var(&o.output, "output", "")
	required := []string{flagWorkspaceID, flagRepository, flagPullRequest}
	if sub == estimateSubmit {
		fs.StringVar(&o.infracost, flagInfracost, "", "")
		fs.StringVar(&o.commit, "commit", "", "")
		fs.StringVar(&o.url, "url", "", "")
		fs.StringVar(&o.infracostVersion, "infracost-version", "", "")
		fs.StringVar(&o.failAbove, "fail-above", "", "")
		required = append(required, flagInfracost)
	} else {
		fs.StringVar(&o.state, flagState, "", "")
		required = append(required, flagPlan, flagState)
	}
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, errHelp
		}
		return o, &usageError{msg: unprefixed(err)}
	}
	if fs.NArg() > 0 {
		return o, usagef("unexpected argument %q: every argument is a --flag", fs.Arg(0))
	}
	var missing []string
	for _, name := range required {
		if strings.TrimSpace(fs.Lookup(name).Value.String()) == "" {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) > 0 {
		return o, usagef("missing %s", strings.Join(missing, ", "))
	}
	if n, err := strconv.ParseInt(o.pullRequest, 10, 64); !pullRequestPattern.MatchString(o.pullRequest) || err != nil || n > math.MaxInt32 {
		return o, usagef("--pull-request takes a pull request number, not %q", o.pullRequest)
	}
	if o.url != "" && !strings.HasPrefix(o.url, "https://") {
		return o, usagef("--url must start with https://")
	}
	if o.failAbove != "" && !failAbovePattern.MatchString(o.failAbove) {
		return o, usagef("--fail-above takes a non-negative amount such as 500 or 99.50, not %q", o.failAbove)
	}
	return o, nil
}

// estimate runs estimate submit or estimate apply. Neither marks its request as an agent's: a CI
// job is no agent, and the setting that refuses agents' writes must never refuse it.
func (c *cli) estimate(ctx context.Context, argv []string, output outputFormat) int {
	if len(argv) == 0 {
		c.warn("costfluent: estimate takes a subcommand, submit or apply\n\n" + estimateHelp)
		return exitUsage
	}
	sub := argv[0]
	switch sub {
	case "help", "--help", "-help", "-h":
		return c.print(estimateHelp)
	case estimateSubmit, estimateApply:
	default:
		return c.fail(exitUsage, fmt.Sprintf("estimate takes submit or apply, not %q; costfluent estimate --help says how", sub))
	}
	o, err := parseEstimate(sub, argv[1:], output)
	if errors.Is(err, errHelp) {
		return c.print(estimateHelp)
	}
	if err != nil {
		return c.fail(exitUsage, err.Error()+"; costfluent estimate --help lists its flags")
	}
	client, err := c.client(ctx)
	if err != nil {
		return c.fail(accessExit(err), err.Error())
	}
	if sub == estimateSubmit {
		return c.submitEstimate(ctx, client, o)
	}
	return c.applyEstimate(ctx, client, o)
}

// document reads the JSON object a flag names; on failure it returns the exit status the failure
// has been reported with.
func (c *cli) document(flagName, path string) (map[string]any, int) {
	data, err := os.ReadFile(path) //nolint:gosec // reading the file the user named is the command's job
	if err != nil {
		return nil, c.fail(exitUsage, fmt.Sprintf("--%s: %v", flagName, err))
	}
	doc, err := estimate.Decode(data)
	if err != nil {
		return nil, c.fail(exitUsage, fmt.Sprintf("--%s (%s) is %v", flagName, path, err))
	}
	return doc, exitOK
}

// decodeBody reads a program's output into the SDK's request, refusing a member the request does
// not carry, so a program that grows one fails here rather than posting without it.
func decodeBody(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("the converted document does not fit the request: %w", err)
	}
	return nil
}

func (c *cli) submitEstimate(ctx context.Context, client *costfluent.Client, o estimateOptions) int {
	infracost, code := c.document(flagInfracost, o.infracost)
	if code != exitOK {
		return code
	}
	var plan map[string]any
	if o.plan != "" {
		if plan, code = c.document(flagPlan, o.plan); code != exitOK {
			return code
		}
	}
	raw, err := estimate.Estimate(infracost, plan, estimate.Args{
		WorkspaceID: o.workspaceID, Repository: o.repository, PullRequest: o.pullRequest,
		Commit: o.commit, URL: o.url, SourceVersion: o.infracostVersion,
	})
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	var input costfluent.SubmitCostEstimateInput
	if err := decodeBody(raw, &input); err != nil {
		return c.fail(exitFailed, err.Error())
	}
	if len(input.Resources) == 0 {
		c.note(fmt.Sprintf("No resource's cost changes in %s: nothing to post.", o.infracost))
		return exitOK
	}
	receipt, err := client.SubmitCostEstimate(ctx, &input)
	if err != nil {
		return c.failure(server.ToolError(err).Error())
	}
	outcome := "updated"
	if receipt.Created {
		outcome = "created"
	}
	c.note(fmt.Sprintf("Posted the cost estimate %s for %s#%s: %d resources, %s.",
		receipt.ID, o.repository, o.pullRequest, len(input.Resources), outcome))
	if code := c.printReceipt(receipt, o.output); code != exitOK {
		return code
	}
	if o.failAbove == "" {
		return exitOK
	}
	return c.judge(receipt, o.failAbove, input.Currency)
}

// judge compares the planned monthly change with --fail-above as exact decimals: a float rounds
// amounts with more than 15 or so significant digits, and the API's carry 8 decimal places beside
// any number of whole ones.
func (c *cli) judge(receipt *costfluent.CostEstimateReceipt, failAbove, currency string) int {
	var change *big.Rat
	if receipt.Planned != nil {
		change, _ = receipt.Planned.MonthlyChange.Rat()
	}
	if change == nil {
		// Failing here would fail every pull request until the API answers with totals, so the gate is
		// left open, loudly.
		c.note("warning: --fail-above is not applied: Costfluent answered without the planned totals, so the change cannot be judged")
		return exitOK
	}
	// failAbovePattern admits only what SetString parses.
	limit, _ := new(big.Rat).SetString(failAbove)
	planned := receipt.Planned.MonthlyChange.String()
	if change.Cmp(limit) > 0 {
		return c.fail(exitThreshold, fmt.Sprintf("the planned monthly change, %s %s, is above --fail-above, %s %s", planned, currency, failAbove, currency))
	}
	c.note(fmt.Sprintf("The planned monthly change, %s %s, is not above --fail-above, %s %s.", planned, currency, failAbove, currency))
	return exitOK
}

func (c *cli) applyEstimate(ctx context.Context, client *costfluent.Client, o estimateOptions) int {
	plan, code := c.document(flagPlan, o.plan)
	if code != exitOK {
		return code
	}
	state, code := c.document(flagState, o.state)
	if code != exitOK {
		return code
	}
	// parseEstimate admits only a number that fits.
	pullRequest, _ := strconv.Atoi(o.pullRequest)
	applied, err := estimate.Apply(plan, state, o.workspaceID, o.repository, pullRequest)
	switch {
	case errors.Is(err, estimate.ErrNotAPlan):
		return c.fail(exitFailed, fmt.Sprintf("--%s (%s) is %v", flagPlan, o.plan, err))
	case errors.Is(err, estimate.ErrNotAState):
		return c.fail(exitFailed, fmt.Sprintf("--%s (%s) is %v", flagState, o.state, err))
	case err != nil:
		return c.fail(exitFailed, err.Error())
	}
	for _, address := range applied.Missing {
		c.note(fmt.Sprintf("warning: %s is created or replaced by the plan, but the state holds no id for it; it is left out", address))
	}
	var input costfluent.ApplyCostEstimateInput
	if err := decodeBody(applied.Body, &input); err != nil {
		return c.fail(exitFailed, err.Error())
	}
	if len(input.Resources) > maxAppliedResources {
		return c.fail(exitFailed, fmt.Sprintf("the plan creates or replaces %d resources with ids, more than the %d one apply takes",
			len(input.Resources), maxAppliedResources))
	}
	receipt, err := client.ApplyCostEstimate(ctx, &input)
	if apiErr := (*costfluent.APIError)(nil); costfluent.IsNotFound(err) && errors.As(err, &apiErr) {
		// A pull request whose plan changed no priced resource posted no estimate, so this is no
		// failure. The API's reason is printed: a mistyped workspace answers 404 too.
		c.note(fmt.Sprintf("No cost estimate for %s#%s in this workspace: nothing to apply (%s).", o.repository, o.pullRequest, apiErr.Message()))
		return exitOK
	}
	if err != nil {
		return c.failure(server.ToolError(err).Error())
	}
	c.note(fmt.Sprintf("Recorded the apply of the cost estimate %s for %s#%s: %d created or replaced resources with ids.",
		receipt.ID, o.repository, o.pullRequest, len(input.Resources)))
	return c.printReceipt(receipt, o.output)
}

func (c *cli) printReceipt(receipt *costfluent.CostEstimateReceipt, output outputFormat) int {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	return c.writeResult(raw, output)
}

// note writes one line to stderr, beside the result on stdout. Its values come from the pull
// request and the API, so it is escaped as an error is.
func (c *cli) note(msg string) {
	c.warn(escape(msg) + "\n")
}
