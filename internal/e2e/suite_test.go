// Run the full e2e scenario suite against the in-process harness.
package e2e

import (
	"testing"
)

// TestCoreScenarios runs every core scenario once.
func TestCoreScenarios(t *testing.T) {
	suite := &Suite{}
	suite.Run(t, Scenarios()...)
}
