package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/friesencr/transcoop/internal/farm"
)

type Options struct {
	Queue, HandBrake                             string
	Preset, PresetFile, OutputDir, KeepLanguages string
	Recursive, FirstAudio                        bool
	Client                                       *farm.Client
	Mappings                                     []farm.Mapping
}

type model struct {
	ctx                                    context.Context
	opts                                   Options
	entries                                []*Entry
	width, height                          int
	file, track                            int
	audioFocus                             bool
	subtitleFocus                          bool
	filter, prompt, input, status, confirm string
	preview                                bool
	scroll                                 int
	busy, dirty                            bool
	history                                [][]trackState
	scans                                  []scanRequest
	scanned, scanFailed, scanTotal         int
	scanError                              string
	folder                                 *folderPlan
	folderIndex                            int
	folderErrors                           []string
	languages                              []string
}
type trackState struct {
	outputs          []Output
	keep             []bool
	changed          bool
	warning          string
	subtitles        []SubtitleOutput
	subtitlesChanged bool
}
type operationMsg struct {
	text string
	err  error
}
type scanResult struct {
	index     int
	audio     []AudioInfo
	subtitles []SubtitleInfo
	err       error
}
type scanRequest struct {
	index, title int
	source       string
}

func Run(ctx context.Context, opts Options) error {
	if opts.KeepLanguages != "" {
		if err := validateLanguages(opts.KeepLanguages); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	local := farm.MapPath(opts.Queue, opts.Mappings)
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	var entries []*Entry
	var folder *folderPlan
	if info.IsDir() {
		fmt.Fprintln(os.Stderr, "Loading preset and finding video files…")
		folder, err = planFolder(ctx, opts)
	} else {
		if opts.Preset != "" || opts.PresetFile != "" || opts.OutputDir != "" || opts.Recursive {
			return fmt.Errorf("preset, output-dir and recursive options require folder input")
		}
		entries, err = Load(local)
	}
	if err != nil {
		return err
	}
	m := newModel(ctx, opts, entries)
	m.folder = folder
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}
func newModel(ctx context.Context, opts Options, entries []*Entry) *model {
	m := &model{ctx: ctx, opts: opts, entries: entries, width: 100, height: 30, status: "Select files, review audio and subtitles, then preview before submitting."}
	if opts.KeepLanguages != "" {
		m.languages = languageList(opts.KeepLanguages)
		Apply(entries, Rule{Kind: "keep-languages", Value: opts.KeepLanguages})
	}
	if opts.FirstAudio {
		Apply(entries, Rule{Kind: "first"})
	}
	return m
}
func (m *model) Init() tea.Cmd {
	if m.folder != nil {
		return m.nextFolderScan()
	}
	return nil
}
func languageList(s string) []string {
	var result []string
	for _, lang := range strings.Split(strings.ToLower(s), ",") {
		if lang = strings.TrimSpace(lang); lang != "" {
			result = append(result, lang)
		}
	}
	return result
}
func (m *model) nextFolderScan() tea.Cmd {
	m.busy = true
	file := m.folder.files[m.folderIndex]
	m.status = fmt.Sprintf("Scanning %d/%d: %s | Ctrl+C: cancel", m.folderIndex+1, len(m.folder.files), file.source)
	return func() tea.Msg {
		entry, err := prepareFolderFile(m.ctx, m.opts.HandBrake, file, m.folder)
		return folderResult{entry, err}
	}
}
func (m *model) visible() []int {
	result := []int{}
	query := strings.ToLower(m.filter)
	for i, e := range m.entries {
		hay := e.Source + " " + e.Destination
		for _, a := range e.Metadata {
			hay += fmt.Sprintf(" %s %s %s %dch %s %s", a.LanguageCode, a.CodecName, a.codec(), a.ChannelCount, a.Name, a.Description)
		}
		if strings.Contains(strings.ToLower(hay), query) {
			result = append(result, i)
		}
	}
	return result
}
func (m *model) targets() []*Entry {
	result := []*Entry{}
	for _, i := range m.visible() {
		if m.entries[i].Selected {
			result = append(result, m.entries[i])
		}
	}
	return result
}
func (m *model) current() *Entry {
	visible := m.visible()
	if len(visible) == 0 {
		return nil
	}
	m.file = max(0, min(m.file, len(visible)-1))
	return m.entries[visible[m.file]]
}
func (m *model) snapshot() {
	snapshot := make([]trackState, len(m.entries))
	for i, e := range m.entries {
		snapshot[i] = trackState{outputs: append([]Output(nil), e.Outputs...), keep: append([]bool(nil), e.Keep...), changed: e.outputsChanged, warning: e.AudioWarning, subtitles: append([]SubtitleOutput(nil), e.Subtitles...), subtitlesChanged: e.subtitlesChanged}
	}
	m.history = append(m.history, snapshot)
	if len(m.history) > 50 {
		m.history = m.history[1:]
	}
	m.dirty = true
}
func (m *model) apply(rule Rule) {
	targets := m.targets()
	if len(targets) == 0 {
		m.status = "Select at least one visible file."
		return
	}
	m.snapshot()
	if rule.Kind == "keep-languages" {
		m.languages = languageList(rule.Value)
	}
	removed, skipped := Apply(targets, rule)
	m.status = fmt.Sprintf("Excluded %d outputs across %d selected files; skipped %d with missing metadata. u: undo", removed, len(targets), skipped)
}
func (m *model) startPrompt(prompt string) { m.prompt = prompt; m.input = "" }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case folderResult:
		file := m.folder.files[m.folderIndex]
		m.folderIndex++
		if msg.err != nil {
			m.folderErrors = append(m.folderErrors, file.source+": "+msg.err.Error())
		} else {
			if m.opts.KeepLanguages != "" {
				Apply([]*Entry{msg.entry}, Rule{Kind: "keep-languages", Value: m.opts.KeepLanguages})
			}
			if m.opts.FirstAudio {
				Apply([]*Entry{msg.entry}, Rule{Kind: "first"})
			}
			m.entries = append(m.entries, msg.entry)
		}
		if m.folderIndex < len(m.folder.files) {
			return m, m.nextFolderScan()
		}
		m.busy = false
		m.dirty = true
		m.status = fmt.Sprintf("Scanned %d files; %d failed. Preset: %s. p: review (including scan errors). Nothing submitted.", len(m.entries), len(m.folderErrors), m.folder.snapshot.Name)
	case operationMsg:
		m.busy = false
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
		} else {
			m.status = msg.text
		}
	case scanResult:
		m.scanned++
		if msg.err != nil {
			m.scanFailed++
			m.scanError = msg.err.Error()
		} else {
			m.entries[msg.index].updateScanMetadata(msg.audio, msg.subtitles)
		}
		if len(m.scans) > 0 {
			return m, m.nextScan()
		}
		m.busy = false
		m.status = fmt.Sprintf("Rescanned %d files; %d failed. Track selections preserved.", m.scanned-m.scanFailed, m.scanFailed)
		if m.scanError != "" {
			m.status += " Last error: " + m.scanError
		}
	case tea.PasteMsg:
		if m.prompt != "" {
			m.input += safe(msg.Content)
		}
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
		}
		if m.confirm != "" {
			if key == "n" || key == "esc" {
				m.confirm = ""
				return m, nil
			}
			if key == "y" {
				which := m.confirm
				m.confirm = ""
				if which == "quit" {
					return m, tea.Quit
				}
				return m, m.submit()
			}
			return m, nil
		}
		if m.prompt != "" {
			switch key {
			case "esc":
				m.prompt = ""
			case "backspace":
				r := []rune(m.input)
				if len(r) > 0 {
					m.input = string(r[:len(r)-1])
				}
			case "ctrl+u":
				m.input = ""
			case "enter":
				return m, m.finishPrompt()
			default:
				m.input += safe(msg.Text)
			}
			return m, nil
		}
		if key == "q" {
			if m.dirty {
				m.confirm = "quit"
			} else {
				return m, tea.Quit
			}
			return m, nil
		}
		if key == "p" || (key == "esc" && m.preview) {
			m.preview = !m.preview
			m.scroll = 0
			return m, nil
		}
		if key == "s" {
			if len(m.targets()) == 0 {
				m.status = "Select at least one visible file."
				return m, nil
			}
			if !m.preview {
				m.preview = true
				m.scroll = 0
				m.status = "Review the full plan below. Press s again to submit."
			} else {
				m.confirm = "submit"
			}
			return m, nil
		}
		if key == "w" {
			m.startPrompt("save")
			m.input = "reviewed-queue.json"
			return m, nil
		}
		if m.preview {
			switch key {
			case "j", "down":
				m.scroll++
			case "k", "up":
				m.scroll = max(0, m.scroll-1)
			case "pgdown":
				m.scroll += max(1, m.height-8)
			case "pgup":
				m.scroll = max(0, m.scroll-max(1, m.height-8))
			case "home":
				m.scroll = 0
			case "end":
				m.scroll = len(m.previewLines())
			}
			return m, nil
		}
		switch key {
		case "tab":
			switch {
			case m.subtitleFocus:
				m.subtitleFocus = false
			case m.audioFocus:
				m.audioFocus = false
				m.subtitleFocus = true
			default:
				m.audioFocus = true
			}
			m.track = 0
		case "t":
			m.subtitleFocus, m.audioFocus, m.track = true, false, 0
		case "j", "down":
			if m.subtitleFocus {
				if e := m.current(); e != nil {
					m.track = min(m.track+1, len(e.Subtitles)-1)
				}
			} else if m.audioFocus {
				if e := m.current(); e != nil {
					m.track = min(m.track+1, len(e.Outputs)-1)
				}
			} else {
				m.file = min(m.file+1, len(m.visible())-1)
				m.track = 0
			}
		case "k", "up":
			if m.audioFocus || m.subtitleFocus {
				m.track = max(0, m.track-1)
			} else {
				m.file = max(0, m.file-1)
				m.track = 0
			}
		case "space", " ", "enter":
			if e := m.current(); e != nil {
				if m.subtitleFocus {
					m.changeSubtitle("keep")
				} else if m.audioFocus {
					if m.track >= 0 && m.track < len(e.Keep) {
						m.snapshot()
						e.Keep[m.track] = !e.Keep[m.track]
						m.status = "Changed this output track only. u: undo"
					}
				} else {
					e.Selected = !e.Selected
				}
			}
		case "d", "o", "i":
			if m.subtitleFocus {
				m.changeSubtitle(map[string]string{"d": "default", "o": "forced", "i": "burn"}[key])
			}
		case "a":
			visible := m.visible()
			all := true
			for _, i := range visible {
				all = all && m.entries[i].Selected
			}
			for _, i := range visible {
				m.entries[i].Selected = !all
			}
		case "/":
			m.startPrompt("filter")
			m.input = m.filter
		case "l":
			m.startPrompt("languages")
		case "x":
			m.startPrompt("exclude")
		case "c":
			m.apply(Rule{Kind: "commentary"})
		case "f":
			m.apply(Rule{Kind: "first"})
		case "b":
			if m.subtitleFocus {
				m.batchSubtitles()
				break
			}
			if reference := m.current(); reference != nil {
				targets := m.targets()
				if len(targets) == 0 {
					m.status = "Select at least one visible file."
					break
				}
				m.snapshot()
				applied, skipped, err := ApplyAudio(reference, targets)
				if err != nil {
					m.status = "Cannot use this file's audio: " + err.Error()
				} else {
					m.status = fmt.Sprintf("Applied audio from %s to %d files; %d skipped (p: details). u: undo", filepath.Base(reference.Source), applied, skipped)
				}
			}
		case "r":
			if m.subtitleFocus {
				m.resetSubtitles()
				break
			}
			m.snapshot()
			for _, e := range m.targets() {
				original, err := Parse(append(append([]byte{'['}, e.Raw...), ']'))
				if err != nil {
					m.status = "Cannot restore imported audio: " + err.Error()
					return m, nil
				}
				e.Outputs = original[0].Outputs
				e.outputsChanged = false
				e.AudioWarning = ""
				for i := range e.Keep {
					e.Keep[i] = true
				}
			}
			m.status = "Restored imported audio settings and selections for selected visible files."
		case "u":
			if len(m.history) > 0 {
				last := m.history[len(m.history)-1]
				m.history = m.history[:len(m.history)-1]
				for i, e := range m.entries {
					e.Keep, e.Outputs = last[i].keep, last[i].outputs
					e.outputsChanged, e.AudioWarning = last[i].changed, last[i].warning
					e.Subtitles, e.subtitlesChanged = last[i].subtitles, last[i].subtitlesChanged
					e.addSubtitleSources()
				}
				m.status = "Undid the last track change."
			}
		case "v":
			return m, m.scan()
		}
	}
	return m, nil
}

func (m *model) finishPrompt() tea.Cmd {
	kind, input := m.prompt, strings.TrimSpace(m.input)
	m.prompt = ""
	switch kind {
	case "filter":
		m.filter = input
		m.file = 0
		m.track = 0
	case "languages":
		if err := validateLanguages(input); err != nil {
			m.status = err.Error()
			return nil
		}
		m.apply(Rule{Kind: "keep-languages", Value: input})
	case "exclude":
		rule, err := ParseRule(input)
		if err != nil {
			m.status = err.Error()
		} else {
			m.apply(rule)
		}
	case "save":
		queue, err := Export(m.targets())
		if err != nil {
			m.status = err.Error()
			return nil
		}
		if input == "" {
			m.status = "Enter an output filename."
			return nil
		}
		m.busy = true
		m.status = "Saving reviewed queue..."
		return func() tea.Msg {
			err := Save(input, queue)
			return operationMsg{text: fmt.Sprintf("Saved %d jobs to %s", len(queue), input), err: err}
		}
	}
	return nil
}
func (m *model) submit() tea.Cmd {
	if m.opts.Client == nil || m.opts.Client.Token == "" {
		m.status = "Pair an operator with transcoop join to submit, or use w to save for later."
		return nil
	}
	targets := m.targets()
	queue, err := Export(targets)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	m.busy = true
	m.status = "Submitting reviewed jobs..."
	return func() tea.Msg {
		var result map[string]int
		_, err := m.opts.Client.JSON(m.ctx, "POST", "/api/jobs", "", queue, &result)
		return operationMsg{text: fmt.Sprintf("Submitted: %d new jobs. Exact duplicates were skipped.", result["imported"]), err: err}
	}
}
func (m *model) scan() tea.Cmd {
	m.scans = nil
	for _, i := range m.visible() {
		e := m.entries[i]
		if e.Selected {
			m.scans = append(m.scans, scanRequest{i, e.Title, farm.MapPath(e.Source, m.opts.Mappings)})
		}
	}
	if len(m.scans) == 0 {
		m.status = "Select at least one visible file."
		return nil
	}
	m.busy = true
	m.scanned, m.scanFailed, m.scanTotal = 0, 0, len(m.scans)
	m.scanError = ""
	return m.nextScan()
}
func (m *model) nextScan() tea.Cmd {
	r := m.scans[0]
	m.scans = m.scans[1:]
	m.status = fmt.Sprintf("Scanning %d/%d (%d failed): %s | Ctrl+C exits", m.scanned+1, m.scanTotal, m.scanFailed, r.source)
	return func() tea.Msg {
		info, err := scanTracks(m.ctx, m.opts.HandBrake, r.source, r.title)
		return scanResult{index: r.index, audio: info.AudioList, subtitles: info.SubtitleList, err: err}
	}
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
func mark(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}
func (m *model) audioLabel(e *Entry, i int) string {
	out := e.Outputs[i]
	info, ok := e.Info(i)
	if !ok {
		return fmt.Sprintf("source #%d | metadata unknown -> %s", out.Source+1, out.Encoder)
	}
	comment := ""
	if info.Commentary() {
		comment = " [commentary]"
	}
	return fmt.Sprintf("source #%d | %s | %s %dch | %s -> %s%s", out.Source+1, info.LanguageCode, info.codec(), info.ChannelCount, info.Name, out.Encoder, comment)
}
func (m *model) warning(e *Entry) string {
	if e.AudioWarning != "" {
		if e.Count() == 0 {
			return "NO AUDIO in output; " + e.AudioWarning
		}
		return e.AudioWarning
	}
	if e.Count() == 0 {
		return "NO AUDIO in output"
	}
	if !e.CompleteMetadata() {
		return "Missing metadata: rescan or review manually"
	}
	var missing []string
	for _, lang := range m.languages {
		found := false
		for i, keep := range e.Keep {
			if keep {
				a, _ := e.Info(i)
				if strings.EqualFold(a.LanguageCode, lang) {
					found = true
				}
			}
		}
		if !found {
			missing = append(missing, lang)
		}
	}
	if len(missing) > 0 {
		return "No selected audio for: " + strings.Join(missing, ", ")
	}
	return ""
}
func (m *model) previewLines() []string {
	lines := []string{}
	for _, failure := range m.folderErrors {
		lines = append(lines, "SKIPPED (scan failed): "+failure)
	}
	for _, e := range m.targets() {
		lines = append(lines, e.Source, "  -> "+e.Destination)
		if e.PresetName != "" {
			lines = append(lines, "  Preset: "+e.PresetName+" (saved snapshot)")
		}
		if warning := m.warning(e); warning != "" {
			lines = append(lines, "  WARNING: "+warning)
		}
		for i := range e.Outputs {
			action := "KEEP  "
			if !e.Keep[i] {
				action = "REMOVE"
			}
			lines = append(lines, "  "+action+" "+m.audioLabel(e, i))
			if e.Keep[i] {
				var settings map[string]json.RawMessage
				if json.Unmarshal(e.Outputs[i].Raw, &settings) == nil {
					var values []string
					for _, key := range audioSettings {
						if value, ok := settings[key]; ok {
							values = append(values, key+"="+string(value))
						}
					}
					// Short groups keep settings readable in a normal terminal.
					for len(values) > 0 {
						n := min(3, len(values))
						lines = append(lines, "    "+strings.Join(values[:n], " | "))
						values = values[n:]
					}
				}
			}
		}
		mode := "reviewed selections"
		if e.PresetName != "" && !e.subtitlesChanged {
			snapshot, _, _ := farm.ReadPresetSnapshot(e.Raw)
			if snapshot != nil && snapshot.Version == 1 {
				mode = "preset defaults; foreign search resolves during encode"
			}
		}
		lines = append(lines, "  SUBTITLES: "+mode)
		for i, s := range e.Subtitles {
			action := "REMOVE"
			if s.Keep {
				action = "KEEP  "
			}
			lines = append(lines, "  "+action+" "+e.subtitleLabel(i))
		}
		if e.subtitleCount() == 0 {
			lines = append(lines, "  No subtitles selected.")
		}
		lines = append(lines, "")
	}
	return lines
}
func (m *model) View() tea.View {
	if m.width < 60 || m.height < 20 {
		v := tea.NewView("transcoop review\nResize to at least 60 columns x 20 rows.\nCtrl+C: exit")
		v.AltScreen = true
		return v
	}
	visible, targets := m.visible(), m.targets()
	noAudio := 0
	for _, e := range targets {
		if e.Count() == 0 {
			noAudio++
		}
	}
	lines := []string{"TRANSCOOP  /  TRACK REVIEW", fmt.Sprintf("%d selected visible / %d visible / %d total | %d with no audio | filter: %s", len(targets), len(visible), len(m.entries), noAudio, m.filter)}
	if m.preview {
		lines = append(lines, "PLAN: audio and subtitle choices for selected visible files.")
		plan := m.previewLines()
		rows := m.height - 8
		m.scroll = max(0, min(m.scroll, max(0, len(plan)-rows)))
		end := min(len(plan), m.scroll+rows)
		lines = append(lines, plan[m.scroll:end]...)
		for len(lines) < m.height-5 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf("Plan lines %d-%d / %d | up/down: scroll | p: back | s: submit | w: save", min(m.scroll+1, len(plan)), end, len(plan)))
	} else {
		fileRows := max(3, (m.height-12)/2)
		audioRows := max(2, m.height-12-fileRows)
		focus := ""
		if !m.audioFocus && !m.subtitleFocus {
			focus = " < focus"
		}
		lines = append(lines, "FILES  space: select | a: toggle visible | /: filter"+focus)
		start := max(0, m.file-fileRows+1)
		end := min(len(visible), start+fileRows)
		for row := start; row < end; row++ {
			e := m.entries[visible[row]]
			cursor := " "
			if row == m.file {
				cursor = ">"
			}
			warning := ""
			if m.warning(e) != "" {
				warning = " !"
			}
			lines = append(lines, fmt.Sprintf("%s %s %d/%d audio, %d subs%s  %s", cursor, mark(e.Selected), e.Count(), len(e.Outputs), e.subtitleCount(), warning, filepath.Base(e.Destination)))
		}
		for i := end - start; i < fileRows; i++ {
			lines = append(lines, "")
		}
		focus = ""
		if m.audioFocus {
			focus = " < focus"
		}
		if m.subtitleFocus {
			lines = append(lines, "SUBTITLES  space: keep | d: default | o: forced only | i: burn in < focus")
		} else {
			lines = append(lines, "AUDIO OUTPUTS  space: include/exclude | t: subtitles"+focus)
		}
		e := m.current()
		used := 0
		if e != nil {
			start = max(0, m.track-audioRows+1)
			count := len(e.Outputs)
			if m.subtitleFocus {
				count = len(e.Subtitles)
			}
			end = min(count, start+audioRows)
			for i := start; i < end; i++ {
				cursor := " "
				if i == m.track {
					cursor = ">"
				}
				if m.subtitleFocus {
					lines = append(lines, fmt.Sprintf("%s %s %s", cursor, mark(e.Subtitles[i].Keep), e.subtitleLabel(i)))
				} else {
					lines = append(lines, fmt.Sprintf("%s %s %s", cursor, mark(e.Keep[i]), m.audioLabel(e, i)))
				}
				used++
			}
		}
		for used < audioRows {
			lines = append(lines, "")
			used++
		}
		if e != nil {
			lines = append(lines, "Source: "+e.Source, "Output: "+e.Destination)
		} else {
			lines = append(lines, "No matching files.", "")
		}
		lines = append(lines, "b: apply current pane to batch | Audio: l: languages | c: commentary | f: first", "x: exclude match | r: reset pane | u: undo | v: rescan | tab: files/audio/subtitles", "p: preview | s: submit | w: save | q: quit | arrows/j/k: move")
	}
	status := m.status
	if m.confirm == "submit" {
		server := "coordinator"
		if m.opts.Client != nil {
			server = m.opts.Client.URL
		}
		status = fmt.Sprintf("Submit %d jobs (%d with NO AUDIO) to %s? y/n", len(targets), noAudio, server)
	}
	if m.confirm == "quit" {
		status = "Close this review and discard in-memory edits? y/n (saved files are retained)"
	}
	if m.prompt != "" {
		label := map[string]string{"filter": "Filter path or metadata (empty clears)", "languages": "Keep languages (eng,jpn,und)", "exclude": "Exclude (lang=spa / codec=ac3 / channels=2 / name=commentary)", "save": "Save selected jobs to NEW file"}[m.prompt]
		status = label + ": " + m.input + "_  [enter / esc]"
	}
	lines = append(lines, status)
	for i, line := range lines {
		lines[i] = ansi.Truncate(safe(line), m.width, "…")
	}
	// All external labels are sanitized before entering the terminal renderer.
	if len(lines) > m.height {
		lines = append(lines[:m.height-1], lines[len(lines)-1])
	}
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	return v
}

func validateLanguages(input string) error {
	for _, lang := range strings.Split(input, ",") {
		lang = strings.TrimSpace(lang)
		if len(lang) != 3 || strings.IndexFunc(lang, func(r rune) bool { return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) }) >= 0 {
			return fmt.Errorf("enter comma-separated three-letter language codes, e.g. eng,jpn,und")
		}
	}
	return nil
}
