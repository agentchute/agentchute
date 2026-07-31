package conformance

import "testing"

// D1 (durable commit ordering). Proves the canonical sequence
// write(tmp) -> fsync(tmp) -> link -> fsync(dir), and that a crash at EVERY step
// leaves the record absent-or-whole, never torn. The load-bearing assertion:
// after a crash AFTER link but BEFORE fsync(dir), the record is not yet durable;
// and there is no state where a record is durable+present but its contents were
// never fsync'd. Catches: linking before fsync -> a record that survives a power
// cut without its body.
func TestD1_FsyncOrdering(t *testing.T) {
	// happy path: full sequence, correct order
	var f fakeFS
	durableCommit(&f, "")
	want := "[write(tmp) -> fsync(tmp) -> link(tmp,final) -> fsync(dir)]"
	if f.orderString() != want {
		t.Fatalf("commit order wrong:\n got %s\nwant %s", f.orderString(), want)
	}
	if p, w := f.survivesWhole(); !p || !w {
		t.Fatalf("after full commit a record must be present+whole; present=%v whole=%v", p, w)
	}

	// crash at each step: the record must be absent-or-whole, never torn.
	for _, crashAt := range []op{opWriteTmp, opFsyncTmp, opLink, opFsyncDir} {
		var c fakeFS
		durableCommit(&c, crashAt)
		present, whole := c.survivesWhole()
		// the only forbidden state: present (durable dir entry) but NOT whole.
		if present && !whole {
			t.Fatalf("crash before %s yields a TORN record (present, contents not fsync'd): %s", crashAt, c.orderString())
		}
		// fsync(tmp) MUST come before link in any sequence that linked.
		if c.linked && !c.tmpFsynced {
			t.Fatalf("crash before %s: linked without fsync(tmp) first (the load-bearing ordering bug)", crashAt)
		}
	}
}
