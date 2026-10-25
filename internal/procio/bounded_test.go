package procio

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Moved from internal/reposerver with the same assertions.
func TestBoundedOutput(t *testing.T) {
	b := Bounded{Limit: 3}
	if _, err := io.WriteString(&b, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(&b, "d"); err != io.ErrShortWrite || !b.Exceeded || b.String() != "abc" {
		t.Fatal("output bound was exceeded")
	}
	if b.Observed != 4 {
		t.Fatalf("observed %d bytes, want every offered byte", b.Observed)
	}
}

// A truncating buffer keeps the bytes that fit and accepts the rest, so the
// writer is never stopped.
func TestBoundedTruncate(t *testing.T) {
	b := Bounded{Limit: 3, Truncate: true}
	for _, s := range []string{"ab", "cd", "ef"} {
		if n, err := io.WriteString(&b, s); n != len(s) || err != nil {
			t.Fatalf("truncating write failed: %d %v", n, err)
		}
	}
	if b.String() != "abc" || !b.Exceeded || b.Observed != 6 {
		t.Fatalf("truncated to %q exceeded=%t observed=%d", b.String(), b.Exceeded, b.Observed)
	}
}

// helperOutput is the size the helper process writes to stdout.
const helperOutput = 100000

func TestMain(m *testing.M) {
	if os.Getenv("PROCIO_TEST_HELPER") == "1" {
		_, _ = os.Stdout.WriteString(strings.Repeat("x", helperOutput))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// os/exec copies a child's output with io.Copy, which prefers the
// destination's ReadFrom. A buffer that exposed bytes.Buffer's ReadFrom
// would take all output past the bound; the child's output must stop at the
// limit and the run must fail.
func TestBoundedLimitsSubprocessOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := &Bounded{Limit: 1000}
	command := exec.Command(executable, "-test.run=^$")
	command.Env = append(os.Environ(), "PROCIO_TEST_HELPER=1")
	command.Stdout = output
	err = command.Run()
	if err == nil || !output.Exceeded || len(output.Bytes()) > 1000 {
		t.Fatalf("subprocess output not bounded: err=%v exceeded=%t buffered=%d", err, output.Exceeded, len(output.Bytes()))
	}
	if _, ok := any(output).(io.ReaderFrom); ok {
		t.Fatal("Bounded must not implement io.ReaderFrom")
	}
}
