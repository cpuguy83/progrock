package ui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fogleman/ease"
	"github.com/muesli/termenv"
)

type Spinner interface {
	tea.Model

	ViewFancy() string
	ViewFrame(SpinnerFrames) (string, time.Time, int)
}

type Rave struct {
	// Show extra details useful for debugging a desynced rave.
	ShowDetails bool

	// The animation to display.
	Frames SpinnerFrames

	// color profile configured at start (to respect NO_COLOR etc)
	colorProfile termenv.Profile

	// the sequence to visualize
	start time.Time
	marks []marker

	// syncing along to music
	track *track

	// refresh rate
	fps float64

	// current position in the marks sequence
	pos int
}

type marker struct {
	Start      float64 `json:"start"`
	Duration   float64 `json:"duration"`
	Confidence float64 `json:"confidence"`
}

var _ Spinner = &Rave{}

var colors = []termenv.Color{
	termenv.ANSIRed,
	termenv.ANSIGreen,
	termenv.ANSIYellow,
	termenv.ANSIBlue,
	termenv.ANSIMagenta,
	termenv.ANSICyan,
}

// DefaultBPM is a sane default of 123 beats per minute.
const DefaultBPM = 123

// SpinnerFrames contains animation frames.
type SpinnerFrames struct {
	Frames []string
	Easing ease.Function
}

var MeterFrames = SpinnerFrames{
	[]string{"█", "█", "▇", "▆", "▅", "▄", "▃", "▂", "▁", " "},
	ease.InOutCubic,
}

var FadeFrames = SpinnerFrames{
	[]string{"█", "█", "▓", "▓", "▒", "▒", "░", "░", " ", " "},
	ease.InOutCubic,
}

var DotFrames = SpinnerFrames{
	[]string{"⣾", "⣷", "⣧", "⣏", "⡟", "⡿", "⢿", "⢻", "⣹", "⣼"},
	ease.Linear,
}

var MiniDotFrames = SpinnerFrames{
	[]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	ease.Linear,
}

func NewRave() *Rave {
	r := &Rave{
		Frames:       MeterFrames,
		colorProfile: ColorProfile(),
	}

	r.reset()

	return r
}

func (rave *Rave) reset() {
	rave.start = time.Now()
	rave.marks = []marker{
		{
			Start:    0,
			Duration: 60.0 / DefaultBPM,
		},
	}
	rave.track = nil
	rave.pos = 0
}

func (rave *Rave) Init() tea.Cmd {
	cmds := []tea.Cmd{
		Frame(rave.fps),
		rave.setFPS(DefaultBPM),
		func() tea.Msg {
			return nil
		},
	}

	return tea.Batch(
		cmds...,
	)
}

func (rave *Rave) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case syncedMsg:
		if msg.playing == nil {
			// nothing playing
			return rave, nil
		}

		// update the new timing
		rave.marks = msg.analysis.Beats

		// the Spotify API is strange: Timestamp doesn't do what it's documented to
		// do. the docs say it returns the server timestamp, but that's not at all
		// true. instead it reflects the last time the state was updated.
		//
		// assuming no user interaction, for the first 30 seconds Timestamp will
		// reflect the time the song started, but after 30 seconds it gets bumped
		// once again without the user doing anything. this doesn't happen for
		// every song.
		//
		// since it's going to be wrong half the time no matter what, let's just
		// use the timestamp value directly. :(
		rave.start = time.UnixMilli(msg.playing.Timestamp)

		// rewind to the beginning in case the song changed
		//
		// NB: this doesn't actually reset the sequence shown to the user, it just
		// affects where we start looping through in Progress
		rave.pos = 0

		// save the playing track to enable fancier UI + music status
		rave.track = msg.playing.Item

		// update the FPS appropriately for the track's average tempo
		return rave, rave.setFPS(msg.analysis.Track.Tempo)

	case syncErrMsg:
		return rave, tea.Printf("sync error: %s", msg.err)

	// NB: these might be hit at an upper layer instead; in which case it will
	// *not* propagate.
	case FrameMsg:
		return rave, Frame(rave.fps)
	case SetFPSMsg:
		rave.fps = float64(msg)
		return rave, nil

	// NB: these are captured and forwarded at the outer level.
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, Keys.Rave):
			return rave, rave.Sync()
		case key.Matches(msg, Keys.EndRave):
			return rave, rave.Desync()
		case key.Matches(msg, Keys.ForwardRave):
			rave.start = rave.start.Add(-100 * time.Millisecond)
			rave.pos = 0 // reset and recalculate
			return rave, nil
		case key.Matches(msg, Keys.BackwardRave):
			rave.start = rave.start.Add(100 * time.Millisecond)
			rave.pos = 0 // reset and recalculate
			return rave, nil
		case key.Matches(msg, Keys.Debug):
			rave.ShowDetails = !rave.ShowDetails
			return rave, nil
		}

		return rave, nil

	default:
		return rave, nil
	}
}

func (rave *Rave) setFPS(bpm float64) tea.Cmd {
	bps := bpm / 60.0
	framesPerBeat := len(rave.Frames.Frames)
	fps := bps * float64(framesPerBeat)
	fps *= 2 // decrease chance of missing a frame due to timing
	rave.fps = fps
	return tea.Cmd(func() tea.Msg {
		return SetFPSMsg(fps)
	})
}

func (rave *Rave) View() string {
	frame, _, _ := rave.ViewFrame(rave.Frames)
	return frame
}

func (rave *Rave) ViewFancy() string {
	frame, now, pos := rave.ViewFrame(rave.Frames)
	if rave.ShowDetails {
		frame += " " + rave.viewDetails(now, pos)
	}

	if rave.track != nil && pos != -1 {
		frame = rave.colorProfile.String(frame).
			Foreground(colors[pos%len(colors)]).
			String()
	}

	return frame
}

func (rave *Rave) ViewFrame(frames SpinnerFrames) (string, time.Time, int) {
	framesPerBeat := len(frames.Frames)

	now := time.Now()

	pos, pct := rave.Progress(now)

	frame := int(frames.Easing(pct) * float64(framesPerBeat))

	// some animations go > 100% or <100%, so be defensive and clamp to the
	// frames since that doesn't actually make sense
	if frame < 0 {
		frame = 0
	} else if frame >= framesPerBeat {
		frame = framesPerBeat - 1
	}

	return frames.Frames[frame], now, pos
}

func (model *Rave) viewDetails(now time.Time, pos int) string {
	var out string

	if model.track != nil {
		for i, artist := range model.track.Artists {
			if i > 0 {
				out += ", "
			}
			out += artist.Name
		}

		out += " - " + model.track.Name
	}

	if pos != -1 {
		mark := model.marks[pos]

		elapsed := time.Duration(mark.Start * float64(time.Second))
		dur := time.Duration(mark.Duration * float64(time.Second))
		bpm := time.Minute / dur

		if out != "" {
			out += " "
		}

		out += fmt.Sprintf(
			"%s %dbpm %.1ffps",
			elapsed.Truncate(time.Second),
			bpm,
			model.fps,
		)
	}

	return out
}

func (sched *Rave) Progress(now time.Time) (int, float64) {
	for i := int(sched.pos); i < len(sched.marks); i++ {
		mark := sched.marks[i]

		start := sched.start.Add(time.Duration(mark.Start * float64(time.Second)))

		dur := time.Duration(mark.Duration * float64(time.Second))

		end := start.Add(dur)
		if start.Before(now) && end.After(now) {
			// found the current beat
			sched.pos = i
			return i, float64(now.Sub(start)) / float64(dur)
		}

		if start.After(now) {
			// in between two beats
			return -1, 0
		}
	}

	// reached the end of the beats; replay
	sched.start = now
	sched.pos = 0

	return -1, 0
}

func (m *Rave) Sync() tea.Cmd {
	return func() tea.Msg {
		return nil
	}
}

func (m *Rave) Desync() tea.Cmd {
	m.reset()
	return nil
}

type currentlyPlaying struct {
	Timestamp int64
	Item      *track `json:"item"`
}

type analysis struct {
	Beats []marker
	Track *track
}

type track struct {
	Tempo   float64
	Name    string
	Artists []artist
}

type artist struct {
	Name string
}

type syncedMsg struct {
	playing  *currentlyPlaying
	analysis *analysis
}

type syncErrMsg struct {
	err error
}

func b64s256(val []byte) string {
	h := sha256.New()
	h.Write(val)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func b64rand(bytes int) (string, error) {
	data := make([]byte, bytes)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(data), nil
}

type FrameMsg time.Time

type SetFPSMsg float64

func Frame(fps float64) tea.Cmd {
	return tea.Tick(time.Duration(float64(time.Second)/fps), func(t time.Time) tea.Msg {
		return FrameMsg(t)
	})
}
