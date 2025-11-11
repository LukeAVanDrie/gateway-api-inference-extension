package framework

import (
	"fmt"
	"testing"

	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/saturationdetector/simulation"
)

// MatrixSuite defines the full scope of the test run.
type MatrixSuite struct {
	Labs      map[LabID]LabFactory
	Scenarios map[ScenarioID]Scenario
	Params    []ConfigParam
}

// RunMatrix executes the verification suite.
func RunMatrix(t *testing.T, suite MatrixSuite) {
	// 1. Index ConfigParams by Matrix Cell
	// Map: "LabID|ScenarioID" -> []ConfigParam
	paramIndex := make(map[string][]ConfigParam)
	for _, p := range suite.Params {
		key := cellKey(p.LabID, p.ScenarioID)
		paramIndex[key] = append(paramIndex[key], p)
	}

	// 2. Iterate over every defined Scenario
	// We run the cross-product of (Labs x Scenarios).
	for lID, labFactory := range suite.Labs {
		for sID, scenario := range suite.Scenarios {

			// Lookup params for this specific cell
			key := cellKey(lID, sID)
			activeParams := paramIndex[key]

			// Define the Test Name: "Lab-Ideal/Step-Response"
			testName := fmt.Sprintf("%s/%s", lID, sID)

			t.Run(testName, func(t *testing.T) {
				// A. Build Phase
				sim := labFactory(scenario.Traffic, activeParams)

				// B. Execute Phase
				// Run the script.
				scenario.Execute(sim)

				// C. Analyze Phase
				// Extract results and grade the run.
				// TODO: Allow overriding Setpoint/Limits per scenario if needed.
				res := sim.GetResults()
				score := Analyze(res, 0.85, 1.0, 0.05)


				t.Log(score.String())
				LogRequestSummary(t, res)
				LogTimeline(t, res.Timeline, 25)

				// D. Sanity Check Phase (Scenario Assertions)
				// This runs even if 0 ConfigParams map to this cell.
				if scenario.Assertions != nil {
					t.Run("Sanity", func(t *testing.T) {
						scenario.Assertions(t, score)
					})
				}

				// E. Requirement Verification Phase (ConfigParams)
				key := cellKey(lID, sID)
				params := paramIndex[key]

				for _, p := range params {
					// Subtest: "ProportionalGain"
					t.Run(p.Name, func(t *testing.T) {
						p.Verify(t, score)
					})
				}
			})
		}
	}
}

func cellKey(l LabID, s ScenarioID) string {
	return fmt.Sprintf("%s|%s", l, s)
}

// LogRequestSummary prints a high-level volume report.
func LogRequestSummary(t *testing.T, res *simulation.SimResult) {
	total := res.TotalRequests
	completed := len(res.CompletedRequests)
	shed := res.ShedRequestCount
	active := total - completed - shed

	pct := func(v int) float64 {
		if total == 0 {
			return 0.0
		}
		return (float64(v) / float64(total)) * 100.0
	}

	t.Logf("\nRequest Summary: Total=%d | ✅ Completed=%d (%.1f%%) | ⛔ Shed=%d (%.1f%%) | ⏳ Active/Buffered=%d (%.1f%%)",
		total,
		completed, pct(completed),
		shed, pct(shed),
		active, pct(active),
	)
}
