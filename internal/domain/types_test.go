package domain

import "testing"

func TestOrderActivityUsesTerminalStatusNotPendingQuantity(t *testing.T) {
	interim := Order{Status: "OPEN PENDING", Quantity: 50, PendingQuantity: 0}
	if !interim.Cancellable() || interim.RemainingQuantity() != 50 {
		t.Fatalf("interim order cancellable=%v remaining=%d", interim.Cancellable(), interim.RemainingQuantity())
	}
	partial := Order{Status: "OPEN", Quantity: 50, FilledQuantity: 20, PendingQuantity: 30}
	if !partial.Cancellable() || partial.RemainingQuantity() != 30 {
		t.Fatalf("partial order cancellable=%v remaining=%d", partial.Cancellable(), partial.RemainingQuantity())
	}
	for _, status := range []string{"COMPLETE", "CANCELLED", "REJECTED"} {
		terminal := Order{Status: status, Quantity: 50, PendingQuantity: 50}
		if terminal.Cancellable() || terminal.RemainingQuantity() != 0 {
			t.Fatalf("terminal %s cancellable=%v remaining=%d", status, terminal.Cancellable(), terminal.RemainingQuantity())
		}
	}
}
