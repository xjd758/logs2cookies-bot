package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/amarnathcjd/gogram/telegram"
)

// MAX_NEST_STREAM_BYTES caps streamed nested-archive reads (not Telegram uploads).
const MAX_NEST_STREAM_BYTES = 16 * 1024 * 1024 * 1024

// Bot wraps a gogram MTProto client (messaging + large downloads).
type Bot struct {
	client *telegram.Client
}

func NewBot(client *telegram.Client) *Bot {
	return &Bot{client: client}
}

type SentMsg struct {
	ChatID int64
	MsgID  int32
}

func (s SentMsg) valid() bool { return s.MsgID != 0 }

func sentFrom(m *telegram.NewMessage) SentMsg {
	if m == nil {
		return SentMsg{}
	}
	return SentMsg{ChatID: m.ChatID(), MsgID: m.ID}
}

func msgRef(chatID int64, msgID int) SentMsg {
	return SentMsg{ChatID: chatID, MsgID: int32(msgID)}
}

func sendOpts() *telegram.SendOptions {
	return &telegram.SendOptions{ParseMode: "markdown"}
}

func (b *Bot) SendText(chatID int64, text string) (*telegram.NewMessage, error) {
	return b.client.SendMessage(chatID, text, sendOpts())
}

func (b *Bot) SendTextWithKeyboard(chatID int64, text string, kb telegram.ReplyMarkup) (*telegram.NewMessage, error) {
	opt := sendOpts()
	opt.ReplyMarkup = kb
	return b.client.SendMessage(chatID, text, opt)
}

func (b *Bot) EditStatus(s SentMsg, text string) {
	if !s.valid() {
		return
	}
	if _, err := b.client.EditMessage(s.ChatID, s.MsgID, text, sendOpts()); err != nil {
		log.Printf("edit status chat=%d: %v", s.ChatID, err)
	}
}

func (b *Bot) EditTextWithKeyboard(chatID int64, msgID int, text string, kb telegram.ReplyMarkup) {
	opt := sendOpts()
	opt.ReplyMarkup = kb
	if _, err := b.client.EditMessage(chatID, int32(msgID), text, opt); err != nil {
		log.Printf("edit keyboard chat=%d: %v", chatID, err)
	}
}

func (b *Bot) EditPlain(chatID int64, msgID int, text string) {
	b.client.EditMessage(chatID, int32(msgID), text, sendOpts())
}

func (b *Bot) DeleteMessage(chatID int64, msgID int) {
	b.client.DeleteMessages(chatID, []int32{int32(msgID)})
}

func (b *Bot) AnswerCallback(cq *telegram.CallbackQuery, text string) {
	if cq == nil {
		return
	}
	cq.Answer(text)
}

func (b *Bot) SendDocument(chatID int64, path, caption string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = b.client.SendMedia(chatID, f, &telegram.MediaOptions{
		Caption:       caption,
		ParseMode:     "markdown",
		ForceDocument: true,
		FileName:      filepath.Base(path),
	})
	return err
}

func (b *Bot) DownloadMessage(msg *telegram.NewMessage, dst string, prog progressFn) error {
	if msg == nil || msg.Document() == nil {
		return fmt.Errorf("missing document")
	}
	doc := msg.Document()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	var lastEdit time.Time
	_, err = msg.Download(&telegram.DownloadOptions{
		Buffer:           out,
		ProgressInterval: 2,
		ProgressCallback: func(pi *telegram.ProgressInfo) {
			if prog == nil {
				return
			}
			if time.Since(lastEdit) < 2*time.Second {
				return
			}
			lastEdit = time.Now()
			prog(pi.Current, pi.TotalSize, pi.CurrentSpeed)
		},
	})
	if err != nil {
		os.Remove(dst)
		return err
	}
	if prog != nil {
		prog(doc.Size, doc.Size, 0)
	}
	return nil
}

func cbBtn(text, data string) telegram.KeyboardButton {
	return telegram.Button.Data(text, data)
}

func inlineKeyboard(rows ...[]telegram.KeyboardButton) telegram.ReplyMarkup {
	kb := telegram.NewKeyboard()
	for _, row := range rows {
		kb.AddRow(row...)
	}
	return kb.Build()
}

func docFileName(doc *telegram.DocumentObj) string {
	if doc == nil {
		return "download"
	}
	for _, attr := range doc.Attributes {
		if fn, ok := attr.(*telegram.DocumentAttributeFilename); ok && fn.FileName != "" {
			return fn.FileName
		}
	}
	return "download"
}

func commandName(m *telegram.NewMessage) string {
	cmd := strings.TrimSpace(m.GetCommand())
	cmd = strings.TrimPrefix(cmd, "/")
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return strings.ToLower(cmd)
}

func telegramEnv() (apiID int, apiHash, token string, err error) {
	if v := strings.TrimSpace(os.Getenv("TELEGRAM_API_ID")); v != "" {
		apiID, _ = strconv.Atoi(v)
	}
	if apiID == 0 {
		if v := strings.TrimSpace(os.Getenv("APP_ID")); v != "" {
			apiID, _ = strconv.Atoi(v)
		}
	}
	apiHash = strings.TrimSpace(os.Getenv("TELEGRAM_API_HASH"))
	if apiHash == "" {
		apiHash = strings.TrimSpace(os.Getenv("APP_HASH"))
	}
	token = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	}
	var missing []string
	if apiID == 0 {
		missing = append(missing, "TELEGRAM_API_ID")
	}
	if apiHash == "" {
		missing = append(missing, "TELEGRAM_API_HASH")
	}
	if token == "" {
		missing = append(missing, "TELEGRAM_BOT_TOKEN")
	}
	if len(missing) > 0 {
		err = fmt.Errorf("missing env: %v (get API ID/hash from https://my.telegram.org/apps)", missing)
	}
	return
}

func runTelegramBot(root string) error {
	apiID, apiHash, token, err := telegramEnv()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	go janitor(root)
	go sessionsJanitor()

	sessionPath := filepath.Join(root, "logs2cookies.session")
	client, err := telegram.NewClient(telegram.ClientConfig{
		AppID:   int32(apiID),
		AppHash: apiHash,
		Session: sessionPath,
	})
	if err != nil {
		return err
	}
	if _, err := client.Conn(); err != nil {
		return err
	}
	if err := client.LoginBot(token); err != nil {
		return err
	}

	me, err := client.GetMe()
	if err != nil {
		return err
	}
	log.Printf("bot online (MTProto/gogram): @%s", me.Username)

	bot := NewBot(client)
	registerHandlers(bot, client)
	client.Idle()
	return nil
}

func registerHandlers(bot *Bot, c *telegram.Client) {
	c.On(telegram.OnMessage, func(m *telegram.NewMessage) error {
		safeHandleMessage(bot, m)
		return nil
	})

	c.On("callback:", func(cq *telegram.CallbackQuery) error {
		safeHandleCallback(bot, cq)
		return nil
	})
}

func safeHandleMessage(bot *Bot, m *telegram.NewMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
			if _, err := m.Reply(fmt.Sprintf("internal error: %v", r), sendOpts()); err != nil {
				log.Printf("error reply after panic: %v", err)
			}
		}
	}()
	log.Printf("message chat=%d user=%d cmd=%q", m.ChatID(), m.SenderID(), commandName(m))
	handle(bot, m)
}

func safeHandleCallback(bot *Bot, cq *telegram.CallbackQuery) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
		}
	}()
	handleCallback(bot, cq)
}
