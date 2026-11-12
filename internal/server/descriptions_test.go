package server

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"testing"
)

// An input whose description lists the values the API takes is all an assistant knows of them, so
// a value the contract adds and the description leaves out is one no assistant asks for. Each row is
// an input whose description lists every value, held to the contract schema of the parameter it
// sends. An input that names a sample of a long list, as a commitment's provider does, is not listed.
func TestInputDescriptionsListEveryValueTheContractTakes(t *testing.T) {
	raw, err := os.ReadFile("../costfluent/testdata/public.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Components struct {
			Schemas map[string]struct {
				Enum       []string `json:"enum"`
				Extensible []string `json:"x-extensible-enum"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		input  any
		field  string
		schema string
	}{
		{ListSavingsItemsInput{}, "Tab", "SavingsQueueTab"},
		{ListSavingsItemsInput{}, "Source", "SavingsItemSource"},
		{ListSavingsItemsInput{}, "Kind", "SavingsItemKind"},
		{ListSavingsItemsInput{}, "Sort", "SavingsQueueSort"},
		{ListCommitmentsInput{}, "Status", "CommitmentExpiryStatus"},
		{KubernetesEfficiencyInput{}, "Level", "KubernetesEfficiencyLevel"},
		{QueryCostsInput{}, "Granularity", "DateBin"},
		{QueryCostsInput{}, "Measure", "CostMeasure"},
	} {
		schema, ok := contract.Components.Schemas[c.schema]
		if !ok {
			t.Errorf("the contract has no schema %s", c.schema)
			continue
		}
		values := append(schema.Enum, schema.Extensible...)
		if len(values) == 0 {
			t.Errorf("the contract's %s lists no values", c.schema)
			continue
		}
		field, ok := reflect.TypeOf(c.input).FieldByName(c.field)
		if !ok {
			t.Errorf("%T has no field %s", c.input, c.field)
			continue
		}
		description := field.Tag.Get("jsonschema")
		for _, value := range values {
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(value) + `\b`).MatchString(description) {
				t.Errorf("%T.%s's description leaves out %q, which %s takes: %q", c.input, c.field, value, c.schema, description)
			}
		}
	}
}
