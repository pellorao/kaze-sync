package client

import (
	"context"
	"testing"

	"kaze-sync/internal/proto"
)

// A file that was deleted locally, and whose place a directory took, conflicts with a newer server version: the
// files of the directory are held (design 6.6) instead of failing with an endless "target changed" error, and
// discarding them gives the place to the server's file; seed 1637 of TestSimulation.
func TestDeletedFileReplacedByDirectoryIsHeldForTheNewerVersion(t *testing.T) {
	e, f, _, g1 := setup(t)
	g2 := second(t, e, f)
	g1.put("d", []byte("v1"))
	g1.scan()
	g2.scan()
	if _, err := g2.f.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	g1.put("d", []byte("v2 by dev1"))
	g1.scan()
	g2.fs.Remove("d")
	g2.put("d/x", []byte("local"))
	for i := 0; i < 6; i++ {
		rep := g2.scan()
		for _, er := range rep.Errors {
			if er.Kind == proto.KindIO { // d/x is refused while d is a file on the server; that is not the failure
				t.Fatalf("round %d: %+v", i, er)
			}
		}
		if g2.f.Ready() {
			if _, err := g2.f.Pull(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, ok := e.serverFile(f, "d"); !ok || string(got) != "v2 by dev1" {
		t.Fatalf("server d = %q %v", got, ok)
	}
	rows, _ := g2.f.DB.held("")
	if len(rows) != 2 { // the deletion of the file (conflict) and the new file below it (type)
		t.Fatalf("held: %+v", rows)
	}
	// One decision on "d" sets the directory aside; the held file below it is gone with it (the next scan drops it).
	if err := g2.f.Resolve(context.Background(), "d", ActDiscard, true); err != nil {
		t.Fatal(err)
	}
	g2.scan()
	if _, err := g2.f.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, err := g2.fs.Stat("d"); err != nil || st.IsDir {
		t.Fatalf("server version not placed: %+v %v", st, err)
	}
}

func TestSimulationSeed1637(t *testing.T) {
	s := newSim(t, 1637)
	s.run(300, true)
	s.settle()
}
