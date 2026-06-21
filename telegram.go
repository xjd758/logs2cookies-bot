package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// MAX_ARCHIVE_BYTES is the Telegram MTProto download cap for bot uploads (2 GB).
const MAX_ARCHIVE_BYTES = 2 * 1024 * 1024 * 1024

type Bot struct {
	ctx    context.Context
	api    *tg.Client
	sender *message.Sender
	dl     *downloader.Downloader
	ul     *uploader.Uploader

	peerMu sync.RWMutex
	peers  map[int64]tg.InputPeerClass
}

type IncomingDoc struct {
	FileName string
	FileSize int64
	TG       *tg.Document
}

type IncomingMsg struct {
	ChatID   int64
	UserID   int64
	MsgID    int
	Text     string
	Document *IncomingDoc
	Command  string
	Peer     tg.InputPeerClass
}

type IncomingCb struct {
	QueryID int64
	Data    string
	UserID  int64
	ChatID  int64
	MsgID   int
}

type SentMsg struct {
	ChatID int64
	MsgID  int
}

func ensureTelegramEnv() error {
	if os.Getenv("APP_ID") == "" {
		if v := strings.TrimSpace(os.Getenv("TELEGRAM_API_ID")); v != "" {
			os.Setenv("APP_ID", v)
		}
	}
	if os.Getenv("APP_HASH") == "" {
		if v := strings.TrimSpace(os.Getenv("TELEGRAM_API_HASH")); v != "" {
			os.Setenv("APP_HASH", v)
		}
	}
	if os.Getenv("BOT_TOKEN") == "" {
		if v := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")); v != "" {
			os.Setenv("BOT_TOKEN", v)
		}
	}
	if os.Getenv("APP_ID") == "" {
		return fmt.Errorf("TELEGRAM_API_ID / APP_ID is required (get it from https://my.telegram.org/apps)")
	}
	if os.Getenv("APP_HASH") == "" {
		return fmt.Errorf("TELEGRAM_API_HASH / APP_HASH is required")
	}
	if os.Getenv("BOT_TOKEN") == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN / BOT_TOKEN is required")
	}
	return nil
}

func runTelegramBot(root string) error {
	if err := ensureTelegramEnv(); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	go janitor(root)
	go sessionsJanitor()

	ctx := context.Background()
	dispatcher := tg.NewUpdateDispatcher()
	var bot *Bot

	logger, _ := zap.NewProduction()
	opts := telegram.Options{
		Logger:         logger,
		UpdateHandler:  dispatcher,
		SessionStorage: &session.FileStorage{Path: filepath.Join(root, "tg-session.json")},
	}

	return telegram.BotFromEnvironment(ctx, opts, func(ctx context.Context, client *telegram.Client) error {
		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		api := client.API()
		bot = &Bot{
			ctx:    ctx,
			api:    api,
			sender: message.NewSender(api),
			dl:     downloader.NewDownloader(),
			ul:     uploader.NewUploader(api),
			peers:  make(map[int64]tg.InputPeerClass),
		}
		log.Printf("bot online (MTProto): @%s", self.Username)

		dispatcher.OnNewMessage(func(ctx context.Context, entities tg.Entities, u *tg.UpdateNewMessage) error {
			m, ok := u.Message.(*tg.Message)
			if !ok || m.Out {
				return nil
			}
			msg := bot.incomingFromTG(entities, m)
			ent := peer.EntitiesFromUpdate(entities)
			if inputPeer, err := ent.ExtractPeer(m.GetPeerID()); err != nil {
				log.Printf("extract peer chat=%d: %v", msg.ChatID, err)
			} else {
				msg.Peer = inputPeer
				bot.rememberPeer(msg.ChatID, inputPeer)
			}
			log.Printf("message chat=%d user=%d cmd=%q len=%d", msg.ChatID, msg.UserID, msg.Command, len(msg.Text))
			go bot.safeHandle(msg)
			return nil
		})

		dispatcher.OnBotCallbackQuery(func(ctx context.Context, entities tg.Entities, u *tg.UpdateBotCallbackQuery) error {
			cb := bot.incomingFromCb(entities, u)
			go bot.safeHandleCallback(cb)
			return nil
		})
		return nil
	}, telegram.RunUntilCanceled)
}

func (b *Bot) safeHandle(m IncomingMsg) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
			if _, err := b.SendTextTo(m.ChatID, m.Peer, fmt.Sprintf("internal error: %v", r)); err != nil {
				log.Printf("error reply after panic: %v", err)
			}
		}
	}()
	handle(b, m)
}

func (b *Bot) safeHandleCallback(cb IncomingCb) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
		}
	}()
	handleCallback(b, cb)
}

func (b *Bot) rememberPeer(chatID int64, peer tg.InputPeerClass) {
	if peer == nil {
		return
	}
	b.peerMu.Lock()
	b.peers[chatID] = peer
	b.peerMu.Unlock()
}

func (b *Bot) peer(chatID int64) tg.InputPeerClass {
	b.peerMu.RLock()
	defer b.peerMu.RUnlock()
	if p, ok := b.peers[chatID]; ok {
		return p
	}
	return &tg.InputPeerUser{UserID: chatID}
}

func (b *Bot) incomingFromTG(entities tg.Entities, m *tg.Message) IncomingMsg {
	peer := peerFromMessage(entities, m)
	chatID := chatIDFromPeer(m.PeerID)
	b.rememberPeer(chatID, peer)

	var userID int64
	if m.FromID != nil {
		if u, ok := m.FromID.(*tg.PeerUser); ok {
			userID = u.UserID
		}
	}

	var doc *IncomingDoc
	if d := docFromMessage(m); d != nil {
		doc = &IncomingDoc{
			FileName: docFilename(d),
			FileSize: d.Size,
			TG:       d,
		}
	}

	cmd, _ := parseCommand(m.Message)
	return IncomingMsg{
		ChatID:   chatID,
		UserID:   userID,
		MsgID:    m.ID,
		Text:     m.Message,
		Document: doc,
		Command:  cmd,
	}
}

func (b *Bot) incomingFromCb(entities tg.Entities, u *tg.UpdateBotCallbackQuery) IncomingCb {
	chatID := chatIDFromPeer(u.Peer)
	peer := peerFromPeerClass(entities, u.Peer)
	b.rememberPeer(chatID, peer)
	return IncomingCb{
		QueryID: u.QueryID,
		Data:    string(u.Data),
		UserID:  u.UserID,
		ChatID:  chatID,
		MsgID:   u.MsgID,
	}
}

func peerFromMessage(entities tg.Entities, m *tg.Message) tg.InputPeerClass {
	return peerFromPeerClass(entities, m.PeerID)
}

func peerFromPeerClass(entities tg.Entities, peer tg.PeerClass) tg.InputPeerClass {
	switch p := peer.(type) {
	case *tg.PeerUser:
		if u, ok := entities.Users[p.UserID]; ok {
			return u.AsInputPeer()
		}
		return &tg.InputPeerUser{UserID: p.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: p.ChatID}
	case *tg.PeerChannel:
		if ch, ok := entities.Channels[p.ChannelID]; ok {
			return ch.AsInputPeer()
		}
		return &tg.InputPeerChannel{ChannelID: p.ChannelID}
	default:
		return nil
	}
}

func chatIDFromPeer(peer tg.PeerClass) int64 {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return p.UserID
	case *tg.PeerChat:
		return p.ChatID
	case *tg.PeerChannel:
		return p.ChannelID
	default:
		return 0
	}
}

func docFromMessage(m *tg.Message) *tg.Document {
	media, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok || media == nil {
		return nil
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		return nil
	}
	return doc
}

func docFilename(d *tg.Document) string {
	for _, attr := range d.Attributes {
		if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
			return fn.FileName
		}
	}
	return "download"
}

func parseCommand(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '/' {
		return "", false
	}
	rest := text[1:]
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.ToLower(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

func styledMD(text string) message.StyledTextOption {
	return html.String(nil, markdownToHTML(text))
}

func markdownToHTML(md string) string {
	var b strings.Builder
	i := 0
	for i < len(md) {
		if md[i] == '\\' && i+1 < len(md) {
			b.WriteByte(md[i+1])
			i += 2
			continue
		}
		if md[i] == '*' {
			j := strings.IndexByte(md[i+1:], '*')
			if j >= 0 {
				b.WriteString("<b>")
				b.WriteString(htmlEscape(md[i+1 : i+1+j]))
				b.WriteString("</b>")
				i += j + 2
				continue
			}
		}
		if md[i] == '`' {
			j := strings.IndexByte(md[i+1:], '`')
			if j >= 0 {
				b.WriteString("<code>")
				b.WriteString(htmlEscape(md[i+1 : i+1+j]))
				b.WriteString("</code>")
				i += j + 2
				continue
			}
		}
		b.WriteByte(md[i])
		i++
	}
	return b.String()
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func (b *Bot) sendPeer(chatID int64, peer tg.InputPeerClass) tg.InputPeerClass {
	if peer != nil {
		return peer
	}
	return b.peer(chatID)
}

func (b *Bot) SendText(chatID int64, text string) (SentMsg, error) {
	return b.SendTextTo(chatID, b.peer(chatID), text)
}

func (b *Bot) SendTextTo(chatID int64, peer tg.InputPeerClass, text string) (SentMsg, error) {
	p := b.sendPeer(chatID, peer)
	updates, err := b.sender.To(p).StyledText(b.ctx, styledMD(text))
	if err != nil {
		log.Printf("styled send failed chat=%d: %v — retrying plain", chatID, err)
		updates, err = b.sender.To(p).Text(b.ctx, text)
		if err != nil {
			return SentMsg{}, err
		}
	}
	id, err := unpack.MessageID(updates, nil)
	return SentMsg{ChatID: chatID, MsgID: id}, err
}

func (b *Bot) SendTextWithKeyboard(chatID int64, text string, kb *tg.ReplyInlineMarkup) (SentMsg, error) {
	updates, err := b.sender.To(b.peer(chatID)).Markup(kb).StyledText(b.ctx, styledMD(text))
	if err != nil {
		return SentMsg{}, err
	}
	id, err := unpack.MessageID(updates, nil)
	return SentMsg{ChatID: chatID, MsgID: id}, err
}

func (b *Bot) EditStatus(s SentMsg, text string) error {
	_, err := b.sender.To(b.peer(s.ChatID)).Edit(s.MsgID).StyledText(b.ctx, styledMD(text))
	return err
}

func (b *Bot) EditTextWithKeyboard(chatID int64, msgID int, text string, kb *tg.ReplyInlineMarkup) error {
	_, err := b.sender.To(b.peer(chatID)).Markup(kb).Edit(msgID).StyledText(b.ctx, styledMD(text))
	return err
}

func (b *Bot) EditPlain(chatID int64, msgID int, text string) error {
	_, err := b.sender.To(b.peer(chatID)).Edit(msgID).StyledText(b.ctx, styledMD(text))
	return err
}

func (b *Bot) DeleteMessage(chatID int64, msgID int) error {
	_, err := b.sender.To(b.peer(chatID)).Revoke().Messages(b.ctx, msgID)
	return err
}

func (b *Bot) AnswerCallback(queryID int64, text string) error {
	_, err := b.api.MessagesSetBotCallbackAnswer(b.ctx, &tg.MessagesSetBotCallbackAnswerRequest{
		QueryID: queryID,
		Message: text,
	})
	return err
}

func (b *Bot) SendDocument(chatID int64, path, caption string) error {
	up, err := b.ul.FromPath(b.ctx, path)
	if err != nil {
		return err
	}
	_, err = b.sender.To(b.peer(chatID)).File(b.ctx, up, styledMD(caption))
	return err
}

func (b *Bot) DownloadDocument(doc *tg.Document, dst string, prog progressFn, maxBytes int64) error {
	if doc == nil {
		return fmt.Errorf("missing document")
	}
	loc := doc.AsInputDocumentFileLocation()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	w := &progressLimitedWriter{
		w:     out,
		total: doc.Size,
		max:   maxBytes,
		cb:    prog,
		start: time.Now(),
	}
	_, err = b.dl.Download(b.api, loc).WithThreads(4).Stream(b.ctx, w)
	if err != nil {
		os.Remove(dst)
		return err
	}
	if w.n > maxBytes {
		os.Remove(dst)
		return fmt.Errorf("file too large (cap %s)", formatBytes(maxBytes))
	}
	if prog != nil {
		prog(w.n, w.total, float64(w.n)/time.Since(w.start).Seconds())
	}
	return nil
}

type progressLimitedWriter struct {
	w     io.Writer
	total int64
	max   int64
	n     int64
	cb    progressFn
	start time.Time
	last  time.Time
}

func (p *progressLimitedWriter) Write(b []byte) (int, error) {
	if p.n >= p.max+1 {
		return 0, fmt.Errorf("file too large")
	}
	room := p.max + 1 - p.n
	if int64(len(b)) > room {
		b = b[:room]
	}
	n, err := p.w.Write(b)
	p.n += int64(n)
	if p.cb != nil && time.Since(p.last) >= 2*time.Second {
		elapsed := time.Since(p.start).Seconds()
		bps := 0.0
		if elapsed > 0 {
			bps = float64(p.n) / elapsed
		}
		p.cb(p.n, p.total, bps)
		p.last = time.Now()
	}
	return n, err
}

func cbBtn(text, data string) tg.KeyboardButtonClass {
	return &tg.KeyboardButtonCallback{Text: text, Data: []byte(data)}
}

func inlineKeyboard(rows ...[]tg.KeyboardButtonClass) *tg.ReplyInlineMarkup {
	out := make([]tg.KeyboardButtonRow, len(rows))
	for i, r := range rows {
		out[i] = tg.KeyboardButtonRow{Buttons: r}
	}
	return &tg.ReplyInlineMarkup{Rows: out}
}
