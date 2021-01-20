package core

import (
	"github.com/armon/circbuf"

	. "gopkg.in/check.v1"
)

type SuiteLocalJob struct{}

var _ = Suite(&SuiteLocalJob{})

func (s *SuiteLocalJob) TestRun(c *C) {
	job := &LocalJob{}
	job.Command = `echo "foo bar"`

	b, _ := circbuf.NewBuffer(1000)
	e := NewExecution()
	e.OutputStream = b

	err := job.Run(&Context{Execution: e})
	c.Assert(err, IsNil)
	c.Assert(b.String(), Equals, "foo bar\n")
}
