package main

import (
	"fmt"
	"strings"

	"github.com/mozziexwz/gameboost/internal/app"
	"github.com/mozziexwz/gameboost/internal/logx"
	"github.com/mozziexwz/gameboost/internal/probe"
	"github.com/mozziexwz/gameboost/internal/store"
)

// Values sent to the frontend. Times are Unix milliseconds; 0 means unset.

type StatusDTO struct {
	State        string   `json:"state"` // idle | starting | running | stopping
	Config       string   `json:"config"`
	SinceUnix    int64    `json:"sinceUnix"`
	Upload       int64    `json:"upload"` // bytes exchanged with the node
	Download     int64    `json:"download"`
	UploadRate   int64    `json:"uploadRate"` // bytes per second
	DownloadRate int64    `json:"downloadRate"`
	SystemProxy  []string `json:"systemProxy"` // system proxy endpoints taken over
	Transport    string   `json:"transport"`   // tcp | udp while accelerating
	LastError    string   `json:"lastError"`
}

type ConfigDTO struct {
	Name     string `json:"name"`
	Profile  string `json:"profile"`
	Entrance string `json:"entrance"`
	UDP      bool   `json:"udp"` // the node also serves UDP
	Error    string `json:"error"`
	Current  bool   `json:"current"`
}

type ConflictDTO struct {
	Message  string `json:"message"`
	Blocking bool   `json:"blocking"`
	Fix      string `json:"fix"` // a fix FixConflict can apply, or ""
}

type OverviewDTO struct {
	Status         StatusDTO     `json:"status"`
	Configs        []ConfigDTO   `json:"configs"`
	Mode           string        `json:"mode"`
	Processes      []string      `json:"processes"`
	ProcessFolders []string      `json:"processFolders"`
	Targets        []string      `json:"targets"`
	ExitTarget     string        `json:"exitTarget"`
	Transport      string        `json:"transport"` // the user's choice: tcp | udp
	Elevated       bool          `json:"elevated"`
	Conflicts      []ConflictDTO `json:"conflicts"`
	LockError      string        `json:"lockError"`
	SettingsError  string        `json:"settingsError"`
	DataDir        string        `json:"dataDir"`
	Version        string        `json:"version"`
}

type ResultDTO struct {
	Text      string  `json:"text"`
	Detail    string  `json:"detail"`
	OK        bool    `json:"ok"`
	AverageMs float64 `json:"averageMs"`
}

type CheckDTO struct {
	ExitIP         string    `json:"exitIP"`
	ExitIPError    string    `json:"exitIPError"`
	EntranceTarget string    `json:"entranceTarget"`
	Entrance       ResultDTO `json:"entrance"`
	ExitTarget     string    `json:"exitTarget"`
	Exit           ResultDTO `json:"exit"`
	DNSError       string    `json:"dnsError"`
	Transport      string    `json:"transport"`
}

type ImportResultDTO struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists"` // a different file with this name is already in configs\
}

type LogLineDTO struct {
	Seq  uint64 `json:"seq"`
	Time string `json:"time"`
	Tag  string `json:"tag"`
	Text string `json:"text"`
}

type ComponentDTO struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Copyright string `json:"copyright"`
	License   string `json:"license"`
	URL       string `json:"url"`
}

type LinkDTO struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type AboutDTO struct {
	Version      string         `json:"version"`
	Copyright    string         `json:"copyright"`
	License      string         `json:"license"`
	Components   []ComponentDTO `json:"components"`
	LicenseTexts []LinkDTO      `json:"licenseTexts"`
}

func toStatusDTO(st app.Status) StatusDTO {
	dto := StatusDTO{
		State: string(st.State), Config: st.Config,
		Upload: st.Upload, Download: st.Download, UploadRate: st.UploadRate, DownloadRate: st.DownloadRate,
		SystemProxy: st.SystemProxy, Transport: st.Transport,
		LastError: st.LastError,
	}
	if !st.Since.IsZero() {
		dto.SinceUnix = st.Since.UnixMilli()
	}
	return dto
}

func toConfigDTOs(list []store.ConfigEntry, current string) []ConfigDTO {
	out := make([]ConfigDTO, 0, len(list))
	for _, c := range list {
		dto := ConfigDTO{Name: c.Name, Profile: c.Profile, Entrance: c.Entrance, UDP: c.UDP, Current: strings.EqualFold(c.Name, current)}
		if c.Err != nil {
			dto.Error = c.Err.Error()
		}
		out = append(out, dto)
	}
	return out
}

func toConflictDTOs(list []app.Conflict) []ConflictDTO {
	out := make([]ConflictDTO, 0, len(list))
	for _, c := range list {
		out = append(out, ConflictDTO{Message: c.Message, Blocking: c.Blocking, Fix: c.Fix})
	}
	return out
}

// toResultDTO shows the latency once: Text has it (or why it failed) and
// Detail only adds the success count and any partial failures.
func toResultDTO(r probe.Result) ResultDTO {
	detail := fmt.Sprintf("成功 %d/%d", r.SuccessCount, r.TotalCount)
	if r.SuccessCount > 0 && r.SuccessCount < r.TotalCount {
		detail += "；部分失败：" + strings.Join(r.Errors, "、")
	}
	return ResultDTO{
		Text: r.Compact(), Detail: detail, OK: r.SuccessCount > 0,
		AverageMs: float64(r.Average.Microseconds()) / 1000,
	}
}

func toCheckDTO(r app.CheckResult) CheckDTO {
	return CheckDTO{
		ExitIP: r.ExitIP, ExitIPError: r.ExitIPError,
		EntranceTarget: r.EntranceTarget, Entrance: toResultDTO(r.Entrance),
		ExitTarget: r.ExitTarget, Exit: toResultDTO(r.Exit), DNSError: r.DNSError, Transport: r.Transport,
	}
}

func toLogLineDTO(l logx.Line) LogLineDTO {
	return LogLineDTO{Seq: l.Seq, Time: l.Time.Format("15:04:05"), Tag: string(l.Tag), Text: l.Text}
}
