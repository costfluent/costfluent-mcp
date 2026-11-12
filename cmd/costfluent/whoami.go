package main

import (
	"context"
	"encoding/json"
	"errors"
)

const whoamiHelp = `Usage: costfluent whoami [--output json|table]

Prints what GET /v1/me says of the token the commands carry, COSTFLUENT_API_TOKEN or else the
sign-in costfluent login kept: its name, type, organization, workspace and capabilities. It never
prints the token.
`

// whoami prints the API's description of the environment's token.
func (c *cli) whoami(ctx context.Context, argv []string, output outputFormat) int {
	_, output, err := parseArgs("whoami", nil, argv, output)
	if errors.Is(err, errHelp) {
		return c.print(whoamiHelp)
	}
	if err != nil {
		return c.fail(exitUsage, err.Error()+"; costfluent whoami --help lists its flags")
	}
	_, me, code := c.me(ctx)
	if code != exitOK {
		return code
	}
	raw, err := json.Marshal(me)
	if err != nil {
		return c.fail(exitFailed, err.Error())
	}
	return c.writeResult(raw, output)
}
