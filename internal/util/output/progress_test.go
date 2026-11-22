package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestSetProgressToStderrRoutesHumanMessages(t *testing.T) {
	var out, errb bytes.Buffer
	c := NewConsole()
	c.Out, c.Err = &out, &errb
	c.SetProgressToStderr(true)
	c.Infof("进度")
	c.Result(true, "完成")
	if out.Len() != 0 {
		t.Fatalf("stdout must stay clean: %q", out.String())
	}
	if !strings.Contains(errb.String(), "进度") || !strings.Contains(errb.String(), "完成") {
		t.Fatalf("progress must go to stderr: %q", errb.String())
	}
	c.SetProgressToStderr(false)
	c.Infof("回到 stdout")
	if !strings.Contains(out.String(), "回到 stdout") {
		t.Fatalf("progress must return to stdout: %q", out.String())
	}
}
