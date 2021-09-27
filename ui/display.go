package ui

import (
	"bytes"
	"container/ring"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/console"
	"github.com/morikuni/aec"
	digest "github.com/opencontainers/go-digest"
	"github.com/tonistiigi/units"
	"github.com/vito/progrock/graph"
	"github.com/vito/vt100"
	"golang.org/x/time/rate"
)

type Reader interface {
	ReadStatus() (*graph.SolveStatus, bool)
}

func Display(phase string, w io.Writer, r Reader) error {
	var c console.Console
	if file, ok := w.(console.File); ok {
		var err error
		c, err = console.ConsoleFromFile(file)
		if err != nil {
			c = nil
		}
	}

	// don't get interrupted; exhaust the channel
	progCtx := context.Background()
	return DisplaySolveStatus(progCtx, phase, c, os.Stderr, r)
}

func DisplaySolveStatus(ctx context.Context, phase string, c console.Console, w io.Writer, r Reader) error {
	modeConsole := c != nil

	if phase == "" {
		phase = "Building"
	}

	disp := &display{c: c, phase: phase}
	printer := &textMux{w: w}

	t := newTrace(w, modeConsole)

	tickerTimeout := 150 * time.Millisecond
	displayTimeout := 100 * time.Millisecond

	if v := os.Getenv("TTY_DISPLAY_RATE"); v != "" {
		if r, err := strconv.ParseInt(v, 10, 64); err == nil {
			tickerTimeout = time.Duration(r) * time.Millisecond
			displayTimeout = time.Duration(r) * time.Millisecond
		}
	}

	var done bool
	ticker := time.NewTicker(tickerTimeout)
	defer ticker.Stop()

	displayLimiter := rate.NewLimiter(rate.Every(displayTimeout), 1)

	width, height := disp.getSize()

	termHeight := height / 2

	ch := make(chan *graph.SolveStatus)
	go proxy(ch, r)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case ss, ok := <-ch:
			if ok {
				t.update(ss, termHeight, width)
			} else {
				done = true
			}
		}

		if modeConsole {
			width, height = disp.getSize()
			if done {
				disp.print(t.displayInfo(), termHeight, width, height, true)
				t.printErrorLogs(c)
				return nil
			} else if displayLimiter.Allow() {
				ticker.Stop()
				ticker = time.NewTicker(tickerTimeout)
				disp.print(t.displayInfo(), termHeight, width, height, false)
			}
		} else {
			if done || displayLimiter.Allow() {
				printer.print(t)
				if done {
					t.printErrorLogs(w)
					return nil
				}
				ticker.Stop()
				ticker = time.NewTicker(tickerTimeout)
			}
		}
	}
}

const termPad = 10

type displayInfo struct {
	startTime      time.Time
	jobs           []*job
	countTotal     int
	countCompleted int
}

type job struct {
	startTime     *time.Time
	completedTime *time.Time
	name          string
	status        string
	hasError      bool
	isCanceled    bool
	vertex        *vertex
	showTerm      bool
}

type trace struct {
	w             io.Writer
	localTimeDiff time.Duration
	vertexes      []*vertex
	byDigest      map[digest.Digest]*vertex
	nextIndex     int
	updates       map[digest.Digest]struct{}
	modeConsole   bool
}

type vertex struct {
	*graph.Vertex
	statuses []*status
	byID     map[string]*status
	indent   string
	index    int

	logs          [][]byte
	logsPartial   bool
	logsOffset    int
	logsBuffer    *ring.Ring // stores last logs to print them on error
	prev          *graph.Vertex
	events        []string
	lastBlockTime *time.Time
	count         int
	statusUpdates map[string]struct{}

	jobs      []*job
	jobCached bool

	term      *vt100.VT100
	termBytes int
	termCount int
}

func (v *vertex) update(c int) {
	if v.count == 0 {
		now := time.Now()
		v.lastBlockTime = &now
	}
	v.count += c
}

type status struct {
	*graph.VertexStatus
}

func newTrace(w io.Writer, modeConsole bool) *trace {
	return &trace{
		byDigest:    make(map[digest.Digest]*vertex),
		updates:     make(map[digest.Digest]struct{}),
		w:           w,
		modeConsole: modeConsole,
	}
}

func (t *trace) triggerVertexEvent(v *graph.Vertex) {
	if v.Started == nil {
		return
	}

	var old graph.Vertex
	vtx := t.byDigest[v.Digest]
	if v := vtx.prev; v != nil {
		old = *v
	}

	changed := false
	if v.Digest != old.Digest {
		changed = true
	}
	if v.Name != old.Name {
		changed = true
	}
	if v.Started != old.Started {
		if v.Started != nil && old.Started == nil || !v.Started.Equal(*old.Started) {
			changed = true
		}
	}
	if v.Completed != old.Completed && v.Completed != nil {
		changed = true
	}
	if v.Cached != old.Cached {
		changed = true
	}
	if v.Error != old.Error {
		changed = true
	}

	if changed {
		vtx.update(1)
		t.updates[v.Digest] = struct{}{}
	}

	t.byDigest[v.Digest].prev = v
}

func (t *trace) update(s *graph.SolveStatus, termHeight, termWidth int) {
	for _, v := range s.Vertexes {
		prev, ok := t.byDigest[v.Digest]
		if !ok {
			t.nextIndex++
			t.byDigest[v.Digest] = &vertex{
				byID:          make(map[string]*status),
				statusUpdates: make(map[string]struct{}),
				index:         t.nextIndex,
			}
			if t.modeConsole {
				t.byDigest[v.Digest].term = vt100.NewVT100(termHeight, termWidth-termPad)
			}
		}
		t.triggerVertexEvent(v)
		if v.Started != nil && (prev == nil || prev.Started == nil) {
			if t.localTimeDiff == 0 {
				t.localTimeDiff = time.Since(*v.Started)
			}
			t.vertexes = append(t.vertexes, t.byDigest[v.Digest])
		}
		// allow a duplicate initial vertex that shouldn't reset state
		if !(prev != nil && prev.Started != nil && v.Started == nil) {
			t.byDigest[v.Digest].Vertex = v
		}
		t.byDigest[v.Digest].jobCached = false
	}
	for _, s := range s.Statuses {
		v, ok := t.byDigest[s.Vertex]
		if !ok {
			continue // shouldn't happen
		}
		v.jobCached = false
		prev, ok := v.byID[s.ID]
		if !ok {
			v.byID[s.ID] = &status{VertexStatus: s}
		}
		if s.Started != nil && (prev == nil || prev.Started == nil) {
			v.statuses = append(v.statuses, v.byID[s.ID])
		}
		v.byID[s.ID].VertexStatus = s
		v.statusUpdates[s.ID] = struct{}{}
		t.updates[v.Digest] = struct{}{}
		v.update(1)
	}
	for _, l := range s.Logs {
		v, ok := t.byDigest[l.Vertex]
		if !ok {
			continue // shouldn't happen
		}
		v.jobCached = false
		if v.term != nil {
			if v.term.Width != termWidth {
				v.term.Resize(termHeight, termWidth-termPad)
			}
			v.termBytes += len(l.Data)
			v.term.Write(l.Data) // error unhandled on purpose. don't trust vt100
		}
		i := 0
		complete := split(l.Data, byte('\n'), func(dt []byte) {
			if v.logsPartial && len(v.logs) != 0 && i == 0 {
				v.logs[len(v.logs)-1] = append(v.logs[len(v.logs)-1], dt...)
			} else {
				ts := time.Duration(0)
				if v.Started != nil {
					ts = l.Timestamp.Sub(*v.Started)
				}
				prec := 1
				sec := ts.Seconds()
				if sec < 10 {
					prec = 3
				} else if sec < 100 {
					prec = 2
				}
				v.logs = append(v.logs, []byte(fmt.Sprintf("#%d %s %s", v.index, fmt.Sprintf("%.[2]*[1]f", sec, prec), dt)))
			}
			i++
		})
		v.logsPartial = !complete
		t.updates[v.Digest] = struct{}{}
		v.update(1)
	}
}

func (t *trace) printErrorLogs(f io.Writer) {
	for _, v := range t.vertexes {
		if v.Error != "" && !strings.HasSuffix(v.Error, context.Canceled.Error()) {
			fmt.Fprintln(f, "------")
			fmt.Fprintf(f, " > %s:\n", v.Name)
			// tty keeps original logs
			for _, l := range v.logs {
				f.Write(l)
				fmt.Fprintln(f)
			}
			// printer keeps last logs buffer
			if v.logsBuffer != nil {
				for i := 0; i < v.logsBuffer.Len(); i++ {
					if v.logsBuffer.Value != nil {
						fmt.Fprintln(f, string(v.logsBuffer.Value.([]byte)))
					}
					v.logsBuffer = v.logsBuffer.Next()
				}
			}
			fmt.Fprintln(f, "------")
		}
	}
}

func (t *trace) displayInfo() (d displayInfo) {
	d.startTime = time.Now()
	if t.localTimeDiff != 0 {
		d.startTime = (*t.vertexes[0].Started).Add(t.localTimeDiff)
	}
	d.countTotal = len(t.byDigest)
	for _, v := range t.byDigest {
		if v.Completed != nil {
			d.countCompleted++
		}
	}

	for _, v := range t.vertexes {
		if v.jobCached {
			d.jobs = append(d.jobs, v.jobs...)
			continue
		}
		var jobs []*job
		j := &job{
			startTime:     addTime(v.Started, t.localTimeDiff),
			completedTime: addTime(v.Completed, t.localTimeDiff),
			name:          strings.Replace(v.Name, "\t", " ", -1),
			vertex:        v,
		}
		if v.Error != "" {
			if strings.HasSuffix(v.Error, context.Canceled.Error()) {
				j.isCanceled = true
				j.name = "CANCELED " + j.name
			} else {
				j.hasError = true
				j.name = "ERROR " + j.name
			}
		}
		if v.Cached {
			j.name = "CACHED " + j.name
		}
		j.name = v.indent + j.name
		jobs = append(jobs, j)
		for _, s := range v.statuses {
			j := &job{
				startTime:     addTime(s.Started, t.localTimeDiff),
				completedTime: addTime(s.Completed, t.localTimeDiff),
				name:          v.indent + "=> " + s.ID,
			}
			if s.Total != 0 {
				j.status = fmt.Sprintf("%.2f / %.2f", units.Bytes(s.Current), units.Bytes(s.Total))
			} else if s.Current != 0 {
				j.status = fmt.Sprintf("%.2f", units.Bytes(s.Current))
			}
			jobs = append(jobs, j)
		}
		d.jobs = append(d.jobs, jobs...)
		v.jobs = jobs
		v.jobCached = true
	}

	return d
}

func split(dt []byte, sep byte, fn func([]byte)) bool {
	if len(dt) == 0 {
		return false
	}
	for {
		if len(dt) == 0 {
			return true
		}
		idx := bytes.IndexByte(dt, sep)
		if idx == -1 {
			fn(dt)
			return false
		}
		fn(dt[:idx])
		dt = dt[idx+1:]
	}
}

func addTime(tm *time.Time, d time.Duration) *time.Time {
	if tm == nil {
		return nil
	}
	t := (*tm).Add(d)
	return &t
}

type display struct {
	c         console.Console
	phase     string
	lineCount int
	repeated  bool
}

func (disp *display) getSize() (int, int) {
	width := 80
	height := 10
	if disp.c != nil {
		size, err := disp.c.Size()
		if err == nil && size.Width > 0 && size.Height > 0 {
			width = int(size.Width)
			height = int(size.Height)
		}
	}
	return width, height
}

func setupTerminals(jobs []*job, termHeight, height int, all bool) []*job {
	var candidates []*job
	numInUse := 0
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		if j.vertex != nil && j.vertex.termBytes > 0 {
			candidates = append(candidates, j)
		}
		if j.completedTime == nil {
			numInUse++
		}
	}

	if all {
		for i := 0; i < len(candidates); i++ {
			candidates[i].showTerm = true
		}
	} else {
		numFree := height - 2 - numInUse
		numToHide := 0
		termLimit := termHeight + 3

		for i := 0; numFree > termLimit && i < len(candidates); i++ {
			candidates[i].showTerm = true
			numToHide += candidates[i].vertex.term.UsedHeight()
			numFree -= termLimit
		}

		jobs = wrapHeight(jobs, height-2-numToHide)
	}

	return jobs
}

func (disp *display) print(d displayInfo, termHeight, width, height int, all bool) {
	// this output is inspired by Buck
	d.jobs = setupTerminals(d.jobs, termHeight, height, all)
	b := aec.EmptyBuilder
	b = b.Up(uint(disp.lineCount) + 1)
	if !disp.repeated {
		b = b.Down(1)
	}
	disp.repeated = true
	fmt.Fprint(disp.c, b.Column(0).ANSI)

	statusStr := ""
	if d.countCompleted > 0 && d.countCompleted == d.countTotal && all {
		statusStr = "FINISHED"
	}

	fmt.Fprint(disp.c, aec.Hide)
	defer fmt.Fprint(disp.c, aec.Show)

	out := fmt.Sprintf("[+] %s %.1fs (%d/%d) %s", disp.phase, time.Since(d.startTime).Seconds(), d.countCompleted, d.countTotal, statusStr)
	out = align(out, "", width)
	fmt.Fprintln(disp.c, out)
	lineCount := 0
	for _, j := range d.jobs {
		endTime := time.Now()
		if j.completedTime != nil {
			endTime = *j.completedTime
		}
		if j.startTime == nil {
			continue
		}
		dt := endTime.Sub(*j.startTime).Seconds()
		if dt < 0.05 {
			dt = 0
		}
		pfx := " => "
		timer := fmt.Sprintf(" %3.1fs\n", dt)
		status := j.status
		showStatus := false

		left := width - len(pfx) - len(timer) - 1
		if status != "" {
			if left+len(status) > 20 {
				showStatus = true
				left -= len(status) + 1
			}
		}
		if left < 12 { // too small screen to show progress
			continue
		}
		name := j.name
		if len(name) > left {
			name = name[:left]
		}

		out := pfx + name
		if showStatus {
			out += " " + status
		}

		out = align(out, timer, width)
		if j.completedTime != nil {
			color := aec.BlueF
			if j.isCanceled {
				color = aec.YellowF
			} else if j.hasError {
				color = aec.RedF
			}
			out = aec.Apply(out, color)
		}
		fmt.Fprint(disp.c, out)
		lineCount++
		if j.showTerm {
			term := j.vertex.term
			term.Resize(termHeight, width-termPad)
			lineCount += renderTerm(disp.c, term)
			j.vertex.termCount++
			j.showTerm = false
		}
	}
	// override previous content
	if diff := disp.lineCount - lineCount; diff > 0 {
		for i := 0; i < diff; i++ {
			fmt.Fprintln(disp.c, strings.Repeat(" ", width))
		}
		fmt.Fprint(disp.c, aec.EmptyBuilder.Up(uint(diff)).Column(0).ANSI)
	}
	disp.lineCount = lineCount
}

func renderTerm(w io.Writer, term *vt100.VT100) int {
	used := term.UsedHeight()

	lineCount := 0
	for row, l := range term.Content {
		if row+1 > used {
			break
		}

		var lastFormat vt100.Format
		fmt.Fprint(w, "    | ")
		for col, r := range l {
			f := term.Format[row][col]

			if f != lastFormat {
				lastFormat = f
				printFormat(w, f)
			}

			fmt.Fprint(w, string(r))
		}

		// reset to prevent leaking format into the line prefix
		fmt.Fprintln(w, aec.Reset)

		lineCount++
	}

	return lineCount
}

func printFormat(w io.Writer, f vt100.Format) {
	if f == (vt100.Format{}) {
		fmt.Fprint(w, aec.Reset)
		return
	}

	b := aec.EmptyBuilder

	switch f.Fg {
	case vt100.Black:
		b = b.BlackF()
	case vt100.Red:
		b = b.RedF()
	case vt100.Green:
		b = b.GreenF()
	case vt100.Yellow:
		b = b.YellowF()
	case vt100.Blue:
		b = b.BlueF()
	case vt100.Magenta:
		b = b.MagentaF()
	case vt100.Cyan:
		b = b.CyanF()
	case vt100.White:
		b = b.WhiteF()
	}

	switch f.Bg {
	case vt100.Black:
		b = b.BlackB()
	case vt100.Red:
		b = b.RedB()
	case vt100.Green:
		b = b.GreenB()
	case vt100.Yellow:
		b = b.YellowB()
	case vt100.Blue:
		b = b.BlueB()
	case vt100.Magenta:
		b = b.MagentaB()
	case vt100.Cyan:
		b = b.CyanB()
	case vt100.White:
		b = b.WhiteB()
	}

	switch f.Intensity {
	case vt100.Bright:
		b = b.Bold()
	case vt100.Dim:
		b = b.Faint()
	}

	fmt.Fprint(w, b.ANSI)
}

func align(l, r string, w int) string {
	return fmt.Sprintf("%-[2]*[1]s %[3]s", l, w-len(r)-1, r)
}

func wrapHeight(j []*job, limit int) []*job {
	if limit < 0 {
		return nil
	}
	var wrapped []*job
	wrapped = append(wrapped, j...)
	if len(j) > limit {
		wrapped = wrapped[len(j)-limit:]

		// wrap things around if incomplete jobs were cut
		var invisible []*job
		for _, j := range j[:len(j)-limit] {
			if j.completedTime == nil {
				invisible = append(invisible, j)
			}
		}

		if l := len(invisible); l > 0 {
			rewrapped := make([]*job, 0, len(wrapped))
			for _, j := range wrapped {
				if j.completedTime == nil || l <= 0 {
					rewrapped = append(rewrapped, j)
				}
				l--
			}
			freespace := len(wrapped) - len(rewrapped)
			wrapped = append(invisible[len(invisible)-freespace:], rewrapped...)
		}
	}
	return wrapped
}

func proxy(ch chan<- *graph.SolveStatus, r Reader) {
	for {
		status, ok := r.ReadStatus()
		if !ok {
			close(ch)
			return
		}

		ch <- status
	}
}
