package client

import (
	"context"
	"testing"

	"kaze-sync/internal/proto"
)

// A file that was deleted locally, and whose place a directory took, conflicts with a newer server version: the
// directory makes room for it (it is not an endless "target changed" error), seed 1637 of TestSimulation.
func TestDeletedFileReplacedByDirectoryYieldsToNewerVersion(t *testing.T) {
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
	if st, err := g2.fs.Stat("d"); err != nil || st.IsDir {
		t.Fatalf("server version not placed: %+v %v", st, err)
	}
}

func TestSimulationSeed1637(t *testing.T) {
	s := newSim(t, 1637)
	s.run(300, true)
	s.settle()
}
