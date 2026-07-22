package domain

import "testing"

func TestOrderActivityUsesTerminalStatusNotPendingQuantity(t *testing.T) {
	interim := Order{Status: "OPEN PENDING", Quantity: 50, PendingQuantity: 0}
	if !interim.Cancellable() {
		t.Fatal("OPEN PENDING order must remain cancellable even when Kite reports zero pending quantity")
	}
	partial := Order{Status: "OPEN", Quantity: 50, FilledQuantity: 20, PendingQuantity: 30}
	if !partial.Cancellable() {
		t.Fatal("OPEN order must be cancellable")
	}
	for _, status := range []string{"COMPLETE", "CANCELLED", "REJECTED"} {
		terminal := Order{Status: status, Quantity: 50, PendingQuantity: 50}
		if terminal.Cancellable() {
			t.Fatalf("terminal %s was treated as cancellable", status)
		}
	}
}
