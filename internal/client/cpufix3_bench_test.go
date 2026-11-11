package client

import (
	"strconv"
	"testing"

	"kaze-sync/internal/proto"
)

// The cost of HeldItems (called after every task) with a long held list (cf. 29050 held on 2026-11-11).
func BenchmarkCpufix3HeldItems(b *testing.B) {
	a, _ := singlePair(&testing.T{})
	rows := make([]heldRow, 0, 20000)
	for i := 0; i < 20000; i++ {
		rows = append(rows, heldRow{Path: "Projects/some-repo/src/module/file" + strconv.Itoa(i) + ".go", Scope: "d:", Reason: "owner", Kind: "modify", Hash: make([]byte, 32), BaseSeq: 123456, Size: 4096, MtimeNs: 1_700_000_000_000_000_000, At: 1_700_000_000})
	}
	if err := a.f.DB.putHeld(rows); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.f.HeldItems()
	}
	_ = proto.MaxHeldItems
}
