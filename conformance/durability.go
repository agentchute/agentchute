package conformance

import "fmt"

// durability.go models the CANONICAL DURABLE-COMMIT SEQUENCE the v2 decision
// pins for D1:  write(tmp) -> fsync(tmp) -> link(tmp,final) -> fsync(dir).
//
// Why this is its own model and not a method on the in-memory bindings: in-memory
// bindings cannot exercise fsync — durability is a property of the real FS. So we
// model the SEQUENCE explicitly and assert two things the prose-only invariant
// could not: (1) the order is correct, and (2) a crash at each step leaves the
// record ABSENT or WHOLE, never torn / never present-without-contents.
//
// The load-bearing rule: fsync(tmp) MUST precede link. If you link first and
// crash before fsync, the directory entry can survive a power cut pointing at a
// file whose contents were never flushed — a record that appears after reboot
// without its body. That is the exact failure this models.

type op string

const (
	opWriteTmp op = "write(tmp)"
	opFsyncTmp op = "fsync(tmp)"
	opLink     op = "link(tmp,final)"
	opFsyncDir op = "fsync(dir)"
)

// fakeFS records the operation sequence and tracks what would survive a crash
// at the current point: a record is durable+whole only once BOTH its contents
// are fsync'd AND the dir entry is fsync'd.
type fakeFS struct {
	ops           []op
	tmpWritten    bool
	tmpFsynced    bool
	linked        bool
	dirFsynced    bool
}

func (f *fakeFS) do(o op) {
	f.ops = append(f.ops, o)
	switch o {
	case opWriteTmp:
		f.tmpWritten = true
	case opFsyncTmp:
		f.tmpFsynced = true
	case opLink:
		f.linked = true
	case opFsyncDir:
		f.dirFsynced = true
	}
}

// durableCommit performs the canonical sequence. crashBefore (if non-empty)
// simulates a power cut right before that step.
func durableCommit(f *fakeFS, crashBefore op) {
	steps := []op{opWriteTmp, opFsyncTmp, opLink, opFsyncDir}
	for _, s := range steps {
		if s == crashBefore {
			return // power cut
		}
		f.do(s)
	}
}

// survivesWhole reports whether, after a crash at this exact state, the record
// is present AND its contents are durable. The whole point: a linked-but-not-
// fsync'd record must NOT count as a durable, whole record.
func (f *fakeFS) survivesWhole() (present bool, whole bool) {
	present = f.linked && f.dirFsynced // dir entry durable
	whole = present && f.tmpFsynced    // and contents were flushed before linking
	return
}

func (f *fakeFS) orderString() string {
	s := ""
	for i, o := range f.ops {
		if i > 0 {
			s += " -> "
		}
		s += string(o)
	}
	return fmt.Sprintf("[%s]", s)
}
