package service_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"

	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestAlertDeliveryDispatcherDeliversWebhookOnce(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertDeliveryStore(storage.AlertEvent{
		ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
		Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
		RunID: new("run-1"), CommitSHA: new("sha-1"), ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
		Summary: []byte(`{"regressions":1}`), CreatedAt: fixedAlertNow(),
	})
	sender := &fakeAlertDeliverySender{}
	dispatcher := service.NewAlertDeliveryDispatcher(store, sender, fixedAlertNow)

	summary, err := dispatcher.Deliver(ctx, service.AlertDeliveryOptions{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: 10, RetryAfter: time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Enqueued)
	assert.Equal(t, 1, summary.Attempted)
	assert.Equal(t, 1, summary.Delivered)
	require.Len(t, sender.payloads, 1)
	assert.Equal(t, "event-1", sender.payloads[0].Event.ID)
	assert.Equal(t, "https://hooks.example/benchdb", sender.payloads[0].Target)

	summary, err = dispatcher.Deliver(ctx, service.AlertDeliveryOptions{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: 10, RetryAfter: time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, summary.Enqueued)
	assert.Equal(t, 0, summary.Attempted)
	assert.Len(t, sender.payloads, 1, "delivered events must not be sent again")
}

func TestAlertDeliveryDispatcherRecordsFailuresForRetry(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertDeliveryStore(storage.AlertEvent{
		ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
		Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
		ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
		Summary:   []byte(`{"regressions":1}`), CreatedAt: fixedAlertNow(),
	})
	sender := &fakeAlertDeliverySender{err: errors.New("webhook unavailable")}
	dispatcher := service.NewAlertDeliveryDispatcher(store, sender, fixedAlertNow)

	summary, err := dispatcher.Deliver(ctx, service.AlertDeliveryOptions{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: 10, RetryAfter: 2 * time.Minute,
	})
	require.Error(t, err)
	assert.Equal(t, 1, summary.Enqueued)
	assert.Equal(t, 1, summary.Attempted)
	assert.Equal(t, 1, summary.Failed)
	require.Len(t, summary.Failures, 1)
	assert.Equal(t, "event-1", summary.Failures[0].EventID)
	assert.Equal(t, "webhook unavailable", summary.Failures[0].Error)

	summary, err = dispatcher.Deliver(ctx, service.AlertDeliveryOptions{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: 10, RetryAfter: 2 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, summary.Attempted, "failed delivery must wait until next_attempt_at")
	assert.Len(t, sender.payloads, 1)
}

func TestAlertDeliveryDispatcherClaimsImmediatelyBeforeEachSend(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertDeliveryStore(
		storage.AlertEvent{
			ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
			Summary:   []byte(`{"regressions":1}`), CreatedAt: fixedAlertNow(),
		},
		storage.AlertEvent{
			ID: "event-2", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			ReportURL: "https://benchdb.example/ci/report?run_ids=run-2",
			Summary:   []byte(`{"regressions":1}`), CreatedAt: fixedAlertNow().Add(time.Second),
		},
	)
	sender := &fakeAlertDeliverySender{}
	dispatcher := service.NewAlertDeliveryDispatcher(store, sender, fixedAlertNow)

	summary, err := dispatcher.Deliver(ctx, service.AlertDeliveryOptions{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: 2, RetryAfter: time.Minute,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, summary.Attempted)
	assert.Equal(t, 2, summary.Delivered)
	assert.Equal(t, []int32{1, 1}, store.claimLimits, "deliveries should be claimed immediately before each serial send")
	require.Len(t, sender.payloads, 2)
	assert.NotEqual(t, sender.payloads[0].DeliveryID, sender.payloads[1].DeliveryID)
}

func TestWebhookAlertDeliverySenderPostsEventSummaryAsJSON(t *testing.T) {
	ctx := context.Background()
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	sender := service.NewWebhookAlertDeliverySender(server.Client())

	err := sender.SendAlertDelivery(ctx, service.AlertDeliveryPayload{
		DeliveryID: "delivery-1",
		Channel:    service.AlertDeliveryChannelWebhook,
		Target:     server.URL,
		Event: service.AlertDeliveryEvent{
			ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
			Summary:   json.RawMessage(`{"regressions":1}`),
			CreatedAt: fixedAlertNow(),
		},
	})
	require.NoError(t, err)
	event, ok := got["event"].(map[string]any)
	require.True(t, ok)
	summary, ok := event["summary"].(map[string]any)
	require.True(t, ok, "summary must be a JSON object, not base64 text")
	assert.InDelta(t, 1.0, summary["regressions"], 1e-9)
}

func TestSlackAlertDeliverySenderPostsSlackMessage(t *testing.T) {
	ctx := context.Background()
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	sender := service.NewSlackAlertDeliverySender(server.Client())

	err := sender.SendAlertDelivery(ctx, service.AlertDeliveryPayload{
		DeliveryID: "delivery-1",
		Channel:    service.AlertDeliveryChannelSlack,
		Target:     server.URL,
		Event: service.AlertDeliveryEvent{
			ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			RunID: new("run-1"), CommitSHA: new("sha-1"),
			ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
			Summary:   json.RawMessage(`{"regressions":1}`),
			CreatedAt: fixedAlertNow(),
		},
	})
	require.NoError(t, err)
	text, ok := got["text"].(string)
	require.True(t, ok)
	assert.Equal(t, "Benchmark regression at sha-1", text)
	assert.NotContains(t, got, "event", "Slack receives a native message, not the generic webhook envelope")
	blocks, ok := got["blocks"].([]any)
	require.True(t, ok)
	assert.NotEmpty(t, blocks)
}

func TestSlackAlertDeliverySenderDoesNotLinkRelativeReportURL(t *testing.T) {
	ctx := context.Background()
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	sender := service.NewSlackAlertDeliverySender(server.Client())

	err := sender.SendAlertDelivery(ctx, service.AlertDeliveryPayload{
		DeliveryID: "delivery-1",
		Channel:    service.AlertDeliveryChannelSlack,
		Target:     server.URL,
		Event: service.AlertDeliveryEvent{
			ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			ReportURL: "/ci/report?run_ids=run-1",
			Summary:   json.RawMessage(`{"regressions":1}`),
			CreatedAt: fixedAlertNow(),
		},
	})
	require.NoError(t, err)
	blocks, ok := got["blocks"].([]any)
	require.True(t, ok)
	for _, block := range blocks {
		block, ok := block.(map[string]any)
		require.True(t, ok)
		textBlock, ok := block["text"].(map[string]any)
		require.True(t, ok)
		blockText, ok := textBlock["text"].(string)
		require.True(t, ok)
		assert.NotContains(t, blockText, "/ci/report?run_ids=run-1", "a relative report link cannot be opened from Slack")
	}
}

func TestGitHubCheckAlertDeliverySenderCreatesCheckRun(t *testing.T) {
	ctx := context.Background()
	var got map[string]any
	var gotAuth string
	var gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/repos/org/repo/check-runs", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	sender := service.NewGitHubCheckAlertDeliverySender(server.Client(), "ghs_secret", server.URL)

	err := sender.SendAlertDelivery(ctx, service.AlertDeliveryPayload{
		DeliveryID: "delivery-1",
		Channel:    service.AlertDeliveryChannelGitHubCheck,
		Target:     "https://github.com/org/repo",
		Event: service.AlertDeliveryEvent{
			ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			RunID: new("run-1"), CommitSHA: new("sha-1"), Repository: "https://github.com/org/repo",
			ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
			Summary:   json.RawMessage(`{"regressions":1}`),
			CreatedAt: fixedAlertNow(),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer ghs_secret", gotAuth)
	assert.Equal(t, "application/vnd.github+json", gotAccept)
	assert.Equal(t, "BenchDB alert", got["name"])
	assert.Equal(t, "sha-1", got["head_sha"])
	assert.Equal(t, "completed", got["status"])
	assert.Equal(t, "failure", got["conclusion"])
	assert.Equal(t, "event-1", got["external_id"])
	assert.Equal(t, "https://benchdb.example/ci/report?run_ids=run-1", got["details_url"])
	output, ok := got["output"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Benchmark regression in org/repo", output["title"])
	summary, ok := output["summary"].(string)
	require.True(t, ok)
	assert.Contains(t, summary, "[`sha-1`](https://github.com/org/repo/commit/sha-1)")
	assert.Contains(t, summary, "[Open BenchDB report](https://benchdb.example/ci/report?run_ids=run-1)")
}

func TestGitHubCommentAlertDeliverySenderCreatesCommitComment(t *testing.T) {
	ctx := context.Background()
	var got map[string]any
	var gotAuth string
	var gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/repos/org/repo/commits/sha-1/comments", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	sender := service.NewGitHubCommentAlertDeliverySender(server.Client(), "ghs_secret", server.URL)

	err := sender.SendAlertDelivery(ctx, service.AlertDeliveryPayload{
		DeliveryID: "delivery-1",
		Channel:    service.AlertDeliveryChannelGitHubComment,
		Target:     "https://github.com/org/repo",
		Event: service.AlertDeliveryEvent{
			ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			RunID: new("run-1"), CommitSHA: new("sha-1"), Repository: "https://github.com/org/repo",
			ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
			Summary:   json.RawMessage(`{"regressions":1}`),
			CreatedAt: fixedAlertNow(),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer ghs_secret", gotAuth)
	assert.Equal(t, "application/vnd.github+json", gotAccept)
	body, ok := got["body"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(body, "### Benchmark regression in org/repo\n"), body)
	assert.Contains(t, body, "[Open BenchDB report](https://benchdb.example/ci/report?run_ids=run-1)")
	assert.Contains(t, body, "Alert event: `event-1`")
}

func TestEmailAlertDeliverySenderSendsPlainTextMessage(t *testing.T) {
	ctx := context.Background()
	addr, messages := newFakeSMTPServer(t)
	sender := service.NewEmailAlertDeliverySender(addr, "alerts@example.com", "", "")

	err := sender.SendAlertDelivery(ctx, service.AlertDeliveryPayload{
		DeliveryID: "delivery-1",
		Channel:    service.AlertDeliveryChannelEmail,
		Target:     "ops@example.com, dev@example.com",
		Event: service.AlertDeliveryEvent{
			ID: "event-1", RuleID: "rule-1", Kind: storage.AlertEventKindOpened,
			Status: string(service.CIReportStatusFailure), StatusReason: "regressions detected",
			RunID: new("run-1"), CommitSHA: new("sha-1"), Repository: "https://github.com/org/repo",
			ReportURL: "https://benchdb.example/ci/report?run_ids=run-1",
			Summary:   json.RawMessage(`{"regressions":1}`),
			CreatedAt: fixedAlertNow(),
		},
	})
	require.NoError(t, err)
	message := receiveSMTPMessage(t, messages)
	assert.Equal(t, "MAIL FROM:<alerts@example.com>", message.from)
	assert.ElementsMatch(t, []string{"RCPT TO:<ops@example.com>", "RCPT TO:<dev@example.com>"}, message.recipients)
	assert.Contains(t, message.data, "From: alerts@example.com\r\n")
	assert.Contains(t, message.data, "To: ops@example.com, dev@example.com\r\n")
	assert.Contains(t, message.data, "Subject: Benchmark regression in org/repo\r\n")
	assert.Contains(t, message.data, "Report: https://benchdb.example/ci/report?run_ids=run-1")
	assert.Contains(t, message.data, "Repository: https://github.com/org/repo")
	assert.Contains(t, message.data, "Commit: sha-1")
	assert.Contains(t, message.data, "Run: run-1")
}

type smtpTestMessage struct {
	from       string
	recipients []string
	data       string
}

func newFakeSMTPServer(t *testing.T) (string, <-chan smtpTestMessage) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	messages := make(chan smtpTestMessage, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		require.NoError(t, ln.Close())
		<-done
	})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		writeSMTPLine(writer, "220 localhost ESMTP")
		var message smtpTestMessage
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO ") || strings.HasPrefix(upper, "HELO "):
				writeSMTPLine(writer, "250-localhost")
				writeSMTPLine(writer, "250 OK")
			case strings.HasPrefix(upper, "MAIL FROM:"):
				message.from = line
				writeSMTPLine(writer, "250 OK")
			case strings.HasPrefix(upper, "RCPT TO:"):
				message.recipients = append(message.recipients, line)
				writeSMTPLine(writer, "250 OK")
			case upper == "DATA":
				writeSMTPLine(writer, "354 End data with <CR><LF>.<CR><LF>")
				var data strings.Builder
				for {
					dataLine, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(dataLine, "\r\n") == "." {
						break
					}
					data.WriteString(dataLine)
				}
				message.data = data.String()
				messages <- message
				writeSMTPLine(writer, "250 OK")
			case upper == "QUIT":
				writeSMTPLine(writer, "221 Bye")
				return
			default:
				writeSMTPLine(writer, "250 OK")
			}
		}
	}()
	return ln.Addr().String(), messages
}

func writeSMTPLine(writer *bufio.Writer, line string) {
	_, _ = writer.WriteString(line + "\r\n")
	_ = writer.Flush()
}

func receiveSMTPMessage(t *testing.T, messages <-chan smtpTestMessage) smtpTestMessage {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(2 * time.Second):
		stacktest.Dump(t)
		require.FailNow(t, "timed out waiting for SMTP message")
		return smtpTestMessage{}
	}
}

type fakeAlertDeliveryStore struct {
	events      []storage.AlertEvent
	deliveries  map[string]storage.AlertDelivery
	claimLimits []int32
}

func newFakeAlertDeliveryStore(events ...storage.AlertEvent) *fakeAlertDeliveryStore {
	return &fakeAlertDeliveryStore{events: events, deliveries: map[string]storage.AlertDelivery{}}
}

func (s *fakeAlertDeliveryStore) EnqueueAlertDeliveries(
	_ context.Context,
	p storage.EnqueueAlertDeliveriesParams,
) (int, error) {
	enqueued := 0
	for _, event := range s.events {
		key := event.ID + "\x00" + p.Channel + "\x00" + p.Target
		if _, ok := s.deliveries[key]; ok {
			continue
		}
		s.deliveries[key] = storage.AlertDelivery{
			ID: "delivery-" + event.ID, Event: event, EventID: event.ID, Channel: p.Channel, Target: p.Target,
			Status: storage.AlertDeliveryStatusPending, CreatedAt: p.CreatedAt, UpdatedAt: p.CreatedAt,
		}
		enqueued++
	}
	return enqueued, nil
}

func (s *fakeAlertDeliveryStore) ClaimPendingAlertDeliveries(
	_ context.Context,
	p storage.ClaimPendingAlertDeliveriesParams,
) ([]storage.AlertDelivery, error) {
	s.claimLimits = append(s.claimLimits, p.Limit)
	out := make([]storage.AlertDelivery, 0, len(s.deliveries))
	if p.Limit <= 0 {
		return out, nil
	}
	for key, delivery := range s.deliveries {
		if int32(len(out)) >= p.Limit {
			break
		}
		if delivery.Channel != p.Channel || delivery.Target != p.Target {
			continue
		}
		if delivery.Status == storage.AlertDeliveryStatusDelivered {
			continue
		}
		if delivery.NextAttemptAt != nil && delivery.NextAttemptAt.After(p.Now) {
			continue
		}
		now := p.Now
		leaseUntil := p.LeaseUntil
		delivery.AttemptCount++
		delivery.LastAttemptAt = &now
		delivery.NextAttemptAt = &leaseUntil
		delivery.UpdatedAt = now
		s.deliveries[key] = delivery
		out = append(out, delivery)
	}
	return out, nil
}

func (s *fakeAlertDeliveryStore) MarkAlertDeliveryDelivered(
	_ context.Context,
	p storage.MarkAlertDeliveryDeliveredParams,
) (storage.AlertDelivery, error) {
	for key, delivery := range s.deliveries {
		if delivery.ID != p.ID {
			continue
		}
		delivery.Status = storage.AlertDeliveryStatusDelivered
		delivery.LastAttemptAt = &p.AttemptedAt
		delivery.DeliveredAt = &p.AttemptedAt
		delivery.NextAttemptAt = nil
		delivery.LastError = nil
		delivery.UpdatedAt = p.AttemptedAt
		s.deliveries[key] = delivery
		return delivery, nil
	}
	return storage.AlertDelivery{}, storage.ErrNotFound
}

func (s *fakeAlertDeliveryStore) MarkAlertDeliveryFailed(
	_ context.Context,
	p storage.MarkAlertDeliveryFailedParams,
) (storage.AlertDelivery, error) {
	for key, delivery := range s.deliveries {
		if delivery.ID != p.ID {
			continue
		}
		if delivery.Status == storage.AlertDeliveryStatusDelivered {
			return storage.AlertDelivery{}, storage.ErrNotFound
		}
		delivery.Status = storage.AlertDeliveryStatusFailed
		delivery.LastAttemptAt = &p.AttemptedAt
		delivery.NextAttemptAt = &p.NextAttemptAt
		delivery.LastError = &p.Error
		delivery.UpdatedAt = p.AttemptedAt
		s.deliveries[key] = delivery
		return delivery, nil
	}
	return storage.AlertDelivery{}, storage.ErrNotFound
}

type fakeAlertDeliverySender struct {
	err      error
	payloads []service.AlertDeliveryPayload
}

func (s *fakeAlertDeliverySender) SendAlertDelivery(_ context.Context, payload service.AlertDeliveryPayload) error {
	s.payloads = append(s.payloads, payload)
	return s.err
}

func postSlackAlert(t *testing.T, event service.AlertDeliveryEvent) (string, []string) {
	t.Helper()
	var got struct {
		Text   string `json:"text"`
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	err := service.NewSlackAlertDeliverySender(server.Client()).SendAlertDelivery(context.Background(),
		service.AlertDeliveryPayload{Channel: service.AlertDeliveryChannelSlack, Target: server.URL, Event: event})
	require.NoError(t, err)
	blocks := make([]string, 0, len(got.Blocks))
	for _, block := range got.Blocks {
		blocks = append(blocks, block.Text.Text)
	}
	return got.Text, blocks
}

func alertSummaryJSON(t *testing.T, summary service.AlertEventSummary) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(summary)
	require.NoError(t, err)
	return raw
}

func TestSlackAlertSaysWhatRegressedWhereAndByHowMuch(t *testing.T) {
	summary := service.AlertEventSummary{
		Compared: 100, Analyzed: 98, Regressions: 7, Improvements: 36,
		CommitSubject: "Cache <packed> objects & refs",
		Regressed: []service.AlertEventRow{{
			Name: "get-text-first-rate", Machine: "box-a", Unit: "B/s", Before: 1.53e9, After: 1.47e9,
			PercentChange: -3.66, SeriesURL: "https://benchdb.example/series/abc",
		}, {
			Name: "push<new>", Machine: "box-a", Unit: "s", Before: 0.0021, After: 0.0025,
			PercentChange: 19.05,
		}},
	}
	text, blocks := postSlackAlert(t, service.AlertDeliveryEvent{
		ID: "event-1", Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		CommitSHA: new("c71cab86fef397b63d67596695200230af404c7c"), Repository: "https://github.com/org/repo",
		ReportURL: "https://benchdb.example/ci/report?run_ids=run-1", Summary: alertSummaryJSON(t, summary),
	})

	assert.Equal(t, "Benchmark regression in org/repo at c71cab8: get-text-first-rate −3.7% and 6 more", text)
	assert.Equal(t, []string{
		":small_red_triangle_down: *Benchmark regression in org/repo*\n" +
			"Commit <https://github.com/org/repo/commit/c71cab86fef397b63d67596695200230af404c7c|c71cab8>: " +
			"Cache &lt;packed&gt; objects &amp; refs",
		"• <https://benchdb.example/series/abc|get-text-first-rate> on box-a: 1.53 GB/s → 1.47 GB/s (−3.7% vs recent runs)\n" +
			"• push&lt;new&gt; on box-a: 2.1 ms → 2.5 ms (+19.1% vs recent runs)\n" +
			"• and 5 more",
		"In this run: 7 regressed, 36 improved, 55 unchanged, 2 without enough history  ·  " +
			"<https://benchdb.example/ci/report?run_ids=run-1|Open report>",
	}, blocks)
}

func TestSlackAlertForAResolvedRuleSaysItIsBackToNormal(t *testing.T) {
	summary := service.AlertEventSummary{Compared: 98, Analyzed: 98, Improvements: 2}
	text, blocks := postSlackAlert(t, service.AlertDeliveryEvent{
		ID: "event-2", Kind: storage.AlertEventKindResolved, Status: string(service.CIReportStatusSuccess),
		CommitSHA: new("abcdef0123"), Repository: "https://github.com/org/repo",
		ReportURL: "https://benchdb.example/ci/report?run_ids=run-2", Summary: alertSummaryJSON(t, summary),
	})

	assert.Equal(t, "No longer regressed: org/repo at abcdef0", text)
	require.Len(t, blocks, 2)
	assert.True(t, strings.HasPrefix(blocks[0], ":white_check_mark: *No longer regressed: org/repo*"), blocks[0])
	assert.Equal(t, "In this run: 0 regressed, 2 improved, 96 unchanged  ·  "+
		"<https://benchdb.example/ci/report?run_ids=run-2|Open report>", blocks[1])
}

func TestSlackAlertFromAnEventWithOnlyCountsStillNamesTheRepository(t *testing.T) {
	text, blocks := postSlackAlert(t, service.AlertDeliveryEvent{
		ID: "event-3", Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		Repository: "https://github.com/org/repo", Summary: json.RawMessage(`{"regressions":1}`),
	})

	assert.Equal(t, "Benchmark regression in org/repo", text)
	assert.Equal(t, []string{":small_red_triangle_down: *Benchmark regression in org/repo*", "• 1 benchmark regressed"}, blocks)
}

func TestSlackAlertWithLongBenchmarkNamesStaysWithinSlackLimits(t *testing.T) {
	long := strings.Repeat("&", 600)
	rows := make([]service.AlertEventRow, 5)
	for i := range rows {
		rows[i] = service.AlertEventRow{
			Name: long, Machine: long, Unit: "s", Before: 1, After: 2, PercentChange: 100,
			SeriesURL: "https://benchdb.example/series/" + strings.Repeat("f", 32),
		}
	}
	summary := service.AlertEventSummary{Regressions: 5, Compared: 5, Analyzed: 5, CommitSubject: long, Regressed: rows}
	text, blocks := postSlackAlert(t, service.AlertDeliveryEvent{
		ID: "event-4", Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		CommitSHA: new("abcdef0123"), Repository: "https://github.com/org/" + long,
		ReportURL: "https://benchdb.example/ci/report?run_ids=run-4", Summary: alertSummaryJSON(t, summary),
	})

	assert.LessOrEqual(t, len(text), 3000)
	rowLines := 0
	for _, block := range blocks {
		assert.LessOrEqual(t, len(block), 3000, "Slack rejects a section longer than 3000 characters")
		rowLines += strings.Count(block, "• ")
	}
	assert.Equal(t, 5, rowLines, "every row is still delivered")
	assert.Contains(t, blocks[1], "…", "long names are cut short")
}

func TestGitHubCheckConclusionFollowsTheAlertNotItsReport(t *testing.T) {
	for _, tt := range []struct {
		kind, status, want string
	}{
		// A sustained slowdown can open a rule on a report that is clean
		// against its slow parent.
		{storage.AlertEventKindOpened, string(service.CIReportStatusSuccess), "failure"},
		{storage.AlertEventKindResolved, string(service.CIReportStatusSuccess), "success"},
	} {
		t.Run(tt.kind+"/"+tt.status, func(t *testing.T) {
			var got map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(server.Close)
			err := service.NewGitHubCheckAlertDeliverySender(server.Client(), "ghs_secret", server.URL).SendAlertDelivery(
				context.Background(), service.AlertDeliveryPayload{
					Channel: service.AlertDeliveryChannelGitHubCheck, Target: "https://github.com/org/repo",
					Event: service.AlertDeliveryEvent{
						ID: "event-1", Kind: tt.kind, Status: tt.status, CommitSHA: new("sha-1"),
						Repository: "https://github.com/org/repo",
					},
				})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got["conclusion"])
		})
	}
}
