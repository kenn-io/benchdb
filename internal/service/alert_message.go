package service

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.kenn.io/benchdb/internal/storage"
)

// alertMessage is what every delivery channel says about an alert event:
// where it happened, what regressed, and how the rest of the run looked.
type alertMessage struct {
	opened     bool
	repository string
	commitSHA  string
	commitURL  string
	subject    string
	rows       []AlertEventRow
	more       int
	counts     string
	reportURL  string
}

func newAlertMessage(event AlertDeliveryEvent) alertMessage {
	var summary AlertEventSummary
	if len(event.Summary) > 0 {
		// An unreadable summary still leaves the repository, commit, and report.
		_ = json.Unmarshal(event.Summary, &summary)
	}
	m := alertMessage{
		opened:     event.Kind == storage.AlertEventKindOpened,
		repository: shortRepository(event.Repository),
		subject:    summary.CommitSubject,
		rows:       summary.Regressed,
		more:       max(summary.Regressions-len(summary.Regressed), 0),
		counts:     alertRunCounts(summary.CIReportSummary),
	}
	if event.CommitSHA != nil && *event.CommitSHA != "" {
		m.commitSHA = *event.CommitSHA
		if spec, err := githubRepositorySpec(event.Repository); err == nil {
			m.commitURL = "https://github.com/" + spec + "/commit/" + m.commitSHA
		}
	}
	if isAbsoluteHTTPURL(event.ReportURL) {
		m.reportURL = event.ReportURL
	}
	return m
}

func (m alertMessage) headline() string {
	switch {
	case m.opened && m.repository == "":
		return "Benchmark regression"
	case m.opened:
		return "Benchmark regression in " + m.repository
	case m.repository == "":
		return "Benchmark regression resolved"
	default:
		return "No longer regressed: " + m.repository
	}
}

// moreText counts the regressed benchmarks the message does not list.
func (m alertMessage) moreText() string {
	switch {
	case len(m.rows) > 0:
		return fmt.Sprintf("and %d more", m.more)
	case m.more == 1:
		return "1 benchmark regressed"
	default:
		return fmt.Sprintf("%d benchmarks regressed", m.more)
	}
}

func (m alertMessage) shortSHA() string {
	return m.commitSHA[:min(len(m.commitSHA), 7)]
}

// plainText is the message without markup, for email and notification text.
func (m alertMessage) plainText() string {
	var b strings.Builder
	b.WriteString(m.headline())
	if m.commitSHA != "" {
		fmt.Fprintf(&b, "\nCommit %s", m.shortSHA())
		if m.subject != "" {
			fmt.Fprintf(&b, ": %s", m.subject)
		}
	}
	for _, row := range m.rows {
		fmt.Fprintf(&b, "\n- %s on %s: %s", row.Name, row.Machine, row.change())
	}
	if m.more > 0 {
		fmt.Fprintf(&b, "\n- %s", m.moreText())
	}
	if m.counts != "" {
		fmt.Fprintf(&b, "\nIn this run: %s", m.counts)
	}
	if m.reportURL != "" {
		fmt.Fprintf(&b, "\nReport: %s", m.reportURL)
	}
	return b.String()
}

// markdown is the message body for GitHub comments and check runs.
func (m alertMessage) markdown() string {
	var b strings.Builder
	if m.commitSHA != "" {
		sha := "`" + m.shortSHA() + "`"
		if m.commitURL != "" {
			sha = fmt.Sprintf("[%s](%s)", sha, m.commitURL)
		}
		fmt.Fprintf(&b, "Commit %s", sha)
		if m.subject != "" {
			fmt.Fprintf(&b, ": %s", m.subject)
		}
		b.WriteString("\n\n")
	}
	for _, row := range m.rows {
		name := "`" + row.Name + "`"
		if row.SeriesURL != "" {
			name = fmt.Sprintf("[%s](%s)", name, row.SeriesURL)
		}
		fmt.Fprintf(&b, "- %s on %s: %s\n", name, row.Machine, row.change())
	}
	if m.more > 0 {
		fmt.Fprintf(&b, "- %s\n", m.moreText())
	}
	if len(m.rows) > 0 || m.more > 0 {
		b.WriteString("\n")
	}
	if m.counts != "" {
		fmt.Fprintf(&b, "In this run: %s\n\n", m.counts)
	}
	if m.reportURL != "" {
		fmt.Fprintf(&b, "[Open BenchDB report](%s)\n", m.reportURL)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m alertMessage) slack() slackMessage {
	icon := ":white_check_mark:"
	if m.opened {
		icon = ":small_red_triangle_down:"
	}
	header := fmt.Sprintf("%s *%s*", icon, slackEscape(m.headline()))
	if m.commitSHA != "" {
		sha := "`" + m.shortSHA() + "`"
		if m.commitURL != "" {
			sha = fmt.Sprintf("<%s|%s>", m.commitURL, m.shortSHA())
		}
		header += "\nCommit " + sha
		if m.subject != "" {
			header += ": " + slackEscape(m.subject)
		}
	}
	message := slackMessage{Text: slackFallbackText(m), Blocks: []slackBlock{slackSection(header)}}
	if len(m.rows) > 0 || m.more > 0 {
		lines := make([]string, 0, len(m.rows)+1)
		for _, row := range m.rows {
			name := slackEscape(row.Name)
			if row.SeriesURL != "" {
				name = fmt.Sprintf("<%s|%s>", row.SeriesURL, name)
			}
			lines = append(lines, fmt.Sprintf("• %s on %s: %s", name, slackEscape(row.Machine), row.change()))
		}
		if m.more > 0 {
			lines = append(lines, "• "+m.moreText())
		}
		message.Blocks = append(message.Blocks, slackSection(strings.Join(lines, "\n")))
	}
	var footer []string
	if m.counts != "" {
		footer = append(footer, "In this run: "+m.counts)
	}
	if m.reportURL != "" {
		footer = append(footer, fmt.Sprintf("<%s|Open report>", m.reportURL))
	}
	if len(footer) > 0 {
		message.Blocks = append(message.Blocks, slackSection(strings.Join(footer, "  ·  ")))
	}
	return message
}

// slackFallbackText is what Slack shows in notifications, where blocks do
// not render.
func slackFallbackText(m alertMessage) string {
	text := m.headline()
	if m.commitSHA != "" {
		text += " at " + m.shortSHA()
	}
	if len(m.rows) > 0 {
		text += fmt.Sprintf(": %s %s", m.rows[0].Name, signedPercent(m.rows[0].PercentChange))
		if extra := len(m.rows) - 1 + m.more; extra > 0 {
			text += fmt.Sprintf(" and %d more", extra)
		}
	}
	return text
}

func slackSection(text string) slackBlock {
	return slackBlock{Type: "section", Text: slackTextBlock{Type: "mrkdwn", Text: text}}
}

func slackEscape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// change reads like "1.53 GB/s → 1.47 GB/s (−3.6% vs recent runs)".
func (row AlertEventRow) change() string {
	return fmt.Sprintf("%s → %s (%s vs %s)",
		formatAlertValue(row.Before, row.Unit), formatAlertValue(row.After, row.Unit),
		signedPercent(row.PercentChange), row.Against)
}

func signedPercent(value float64) string {
	sign := "+"
	if value < 0 {
		sign = "−"
	}
	return sign + strconv.FormatFloat(math.Abs(value), 'f', 1, 64) + "%"
}

// formatAlertValue scales seconds and byte rates to a readable unit; other
// units keep their value with four significant digits.
func formatAlertValue(value float64, unit string) string {
	switch unit {
	case "s":
		for _, scale := range []struct {
			factor float64
			suffix string
		}{{1, "s"}, {1e-3, "ms"}, {1e-6, "µs"}} {
			if math.Abs(value) >= scale.factor {
				return strconv.FormatFloat(value/scale.factor, 'g', 3, 64) + " " + scale.suffix
			}
		}
		return strconv.FormatFloat(value/1e-9, 'g', 3, 64) + " ns"
	case "B/s":
		for _, scale := range []struct {
			factor float64
			suffix string
		}{{1e9, "GB/s"}, {1e6, "MB/s"}, {1e3, "kB/s"}} {
			if math.Abs(value) >= scale.factor {
				return strconv.FormatFloat(value/scale.factor, 'f', 2, 64) + " " + scale.suffix
			}
		}
		return strconv.FormatFloat(value, 'g', 3, 64) + " B/s"
	}
	formatted := strconv.FormatFloat(value, 'g', 4, 64)
	if unit == "" {
		return formatted
	}
	return formatted + " " + unit
}

func alertRunCounts(summary CIReportSummary) string {
	if summary.Compared == 0 {
		return ""
	}
	unchanged := max(summary.Compared-summary.Regressions-summary.Improvements, 0)
	return fmt.Sprintf("%d regressed, %d improved, %d unchanged", summary.Regressions, summary.Improvements, unchanged)
}

// shortRepository names a GitHub repository as owner/name and leaves other
// repository URLs whole.
func shortRepository(repository string) string {
	if spec, err := githubRepositorySpec(repository); err == nil {
		return spec
	}
	return repository
}
