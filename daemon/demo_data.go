package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"time"
)

// Fictional people: generic names and reserved-looking 1555010xxxx numbers.
type demoPerson struct {
	jid, name string
}

var (
	demoAlex    = demoPerson{"15550100001@s.whatsapp.net", "Alex Rivera"}
	demoMorgan  = demoPerson{"15550100002@s.whatsapp.net", "Morgan Blake"}
	demoSam     = demoPerson{"15550100003@s.whatsapp.net", "Sam Carter"}
	demoJordan  = demoPerson{"15550100004@s.whatsapp.net", "Jordan Lee"}
	demoTaylor  = demoPerson{"15550100005@s.whatsapp.net", "Taylor Kim"}
	demoCasey   = demoPerson{"15550100006@s.whatsapp.net", "Casey Morgan"}
	demoRiley   = demoPerson{"15550100007@s.whatsapp.net", "Riley Quinn"}
	demoJamie   = demoPerson{"15550100008@s.whatsapp.net", "Jamie Patel"}
	demoDrew    = demoPerson{"15550100009@s.whatsapp.net", "Drew Santos"}
	demoMembers = []demoPerson{demoSam, demoJordan, demoTaylor}
)

// demoGroupMember picks the person who answers in the group, by sequence.
func demoGroupMember(seq int) (jid, name string) {
	p := demoMembers[seq%len(demoMembers)]
	return p.jid, p.name
}

// demoMsg is one seeded message. who is nil for my own messages.
type demoMsg struct {
	id        string
	ago       time.Duration
	who       *demoPerson
	text      string
	kind      string
	ack       string // mine only
	quote     string // id of the quoted message
	media     *MediaInfo
	reactions []demoReaction
	edited    string // new text after an edit
	deleted   bool
}

type demoReaction struct {
	who   *demoPerson // nil: me
	emoji string
}

type demoChat struct {
	jid      string
	name     string
	people   []demoPerson
	unread   int
	pinned   bool
	archived bool
	avatar   bool
	msgs     []demoMsg
}

func mins(n int) time.Duration  { return time.Duration(n) * time.Minute }
func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

func p(x demoPerson) *demoPerson { return &x }

func demoChats() []demoChat {
	alex, morgan, sam, jordan, taylor := p(demoAlex), p(demoMorgan), p(demoSam), p(demoJordan), p(demoTaylor)
	casey, riley, jamie, drew := p(demoCasey), p(demoRiley), p(demoJamie), p(demoDrew)
	return []demoChat{
		{jid: demoAlex.jid, name: demoAlex.name, people: []demoPerson{demoAlex}, unread: 2, pinned: true, avatar: true, msgs: []demoMsg{
			{id: "A01", ago: hours(60), who: alex, text: "Hey! Are we still on for the demo next week?"},
			{id: "A02", ago: hours(59), text: "Yes, I booked the room for Thursday.", ack: "read"},
			{id: "A03", ago: hours(59) - mins(5), who: alex, text: "Great, I will bring the slides.", reactions: []demoReaction{{nil, "👍"}}},
			{id: "A04", ago: hours(30), text: "Do you want to go over the agenda first?", ack: "read", reactions: []demoReaction{{alex, "❤️"}}},
			{id: "A05", ago: hours(29), who: alex, text: "Sure. Is 10 am ok?", quote: "A04"},
			{id: "A06", ago: hours(28), text: "10 am works for me.", ack: "read", edited: "10 am works for me, see you there."},
			{id: "A07", ago: hours(5), who: alex, text: "Quick question about the schedule."},
			{id: "A08", ago: hours(2), text: "Sure, ask away.", ack: "delivered"},
			{id: "A09", ago: mins(40), who: alex, text: "Can we move the demo to 11 am?"},
			{id: "A10", ago: mins(38), who: alex, text: "Sorry, something came up."},
		}},
		{jid: demoGroupJID, name: "Weekend Hikers", people: demoMembers, unread: 5, avatar: true, msgs: []demoMsg{
			{id: "G01", ago: hours(50), who: sam, text: "Who is in for Saturday?"},
			{id: "G02", ago: hours(49), who: jordan, text: "I am!"},
			{id: "G03", ago: hours(49) - mins(10), text: "Count me in too.", ack: "read"},
			{id: "G04", ago: hours(48), who: taylor, text: "I will drive, room for three."},
			{id: "G05", ago: hours(20), who: sam, text: "Trail looks great this week.", kind: KindImage, media: &MediaInfo{W: 800, H: 600, Size: 184320, Mime: "image/jpeg"},
				reactions: []demoReaction{{jordan, "😍"}, {taylor, "😍"}, {nil, "🔥"}}},
			{id: "G06", ago: hours(19), text: "Wow, that view!", ack: "delivered", quote: "G05"},
			{id: "G07", ago: hours(4), who: jordan, text: "Should we pack lunch or eat at the lodge?"},
			{id: "G08", ago: hours(3), who: taylor, text: "Lunch at the lodge, they have good soup."},
			{id: "G09", ago: hours(2), who: sam, text: "Meeting point is the north parking lot."},
			{id: "G10", ago: mins(55), who: jordan, text: "Bring water and snacks."},
			{id: "G11", ago: mins(20), who: taylor, text: "Weather looks clear all day."},
		}},
		{jid: demoMorgan.jid, name: demoMorgan.name, people: []demoPerson{demoMorgan}, unread: 1, msgs: []demoMsg{
			{id: "M01", ago: hours(70), who: morgan, text: "Did you see the new release notes?"},
			{id: "M02", ago: hours(69), text: "Not yet, I will read them tonight.", ack: "read"},
			{id: "M03", ago: hours(8), who: morgan, text: "", kind: KindAudio, media: &MediaInfo{Seconds: 14, Size: 112044, Mime: "audio/x-wav", PTT: true}},
			{id: "M04", ago: hours(7), text: "Thanks, listening now.", ack: "read"},
			{id: "M05", ago: hours(1), who: morgan, text: "Let me know what you think."},
		}},
		{jid: demoCasey.jid, name: demoCasey.name, people: []demoPerson{demoCasey}, avatar: true, msgs: []demoMsg{
			{id: "C01", ago: hours(40), text: "Can you send me the report?", ack: "read"},
			{id: "C02", ago: hours(39), who: casey, text: "Quarterly-report.pdf", kind: KindDocument, media: &MediaInfo{Size: 248311, Mime: "application/pdf"}},
			{id: "C03", ago: hours(38), text: "Got it, thank you!", ack: "read", reactions: []demoReaction{{casey, "🙏"}}},
			{id: "C04", ago: hours(12), who: casey, text: "No problem. Ping me if anything looks off."},
			{id: "C05", ago: hours(11), text: "Will do.", ack: "read"},
		}},
		{jid: demoRiley.jid, name: demoRiley.name, people: []demoPerson{demoRiley}, avatar: true, msgs: []demoMsg{
			{id: "R01", ago: hours(25), who: riley, text: "Happy Friday!"},
			{id: "R02", ago: hours(24), text: "Same to you, any plans?", ack: "read"},
			{id: "R03", ago: hours(23), who: riley, text: "", kind: KindSticker, media: &MediaInfo{W: 256, H: 256, Size: 6144, Mime: "image/png"}},
			{id: "R04", ago: hours(22), text: "Ha, love it.", ack: "read"},
			{id: "R05", ago: hours(6), who: riley, text: "Coffee tomorrow?"},
			{id: "R06", ago: hours(5), text: "Absolutely.", ack: "sent"},
		}},
		{jid: demoJamie.jid, name: demoJamie.name, people: []demoPerson{demoJamie}, archived: true, msgs: []demoMsg{
			{id: "J01", ago: hours(66), who: jamie, text: "Is the old invoice still needed?"},
			{id: "J02", ago: hours(65), text: "No, you can drop it.", ack: "read"},
			{id: "J03", ago: hours(64), who: jamie, text: "Oops, wrong chat.", deleted: true},
			{id: "J04", ago: hours(63), text: "No worries.", ack: "read"},
			{id: "J05", ago: hours(62), who: jamie, text: "Thanks, talk soon."},
		}},
		{jid: demoDrew.jid, name: demoDrew.name, people: []demoPerson{demoDrew}, msgs: []demoMsg{
			{id: "D01", ago: hours(15), who: drew, text: "The package arrived this morning."},
			{id: "D02", ago: hours(14), text: "Awesome, thanks for picking it up.", ack: "read"},
			{id: "D03", ago: hours(13), who: drew, text: "Anytime."},
			{id: "D04", ago: hours(12), text: "I owe you lunch.", ack: "read"},
			{id: "D05", ago: hours(11), who: drew, text: "Deal.", reactions: []demoReaction{{nil, "😄"}}},
		}},
		{jid: "15550100010@s.whatsapp.net", name: "Pat Johnson", people: []demoPerson{{"15550100010@s.whatsapp.net", "Pat Johnson"}}, unread: 3, msgs: []demoMsg{
			{id: "P01", ago: hours(10), who: &demoPerson{"15550100010@s.whatsapp.net", "Pat Johnson"}, text: "Are you free for a call later?"},
			{id: "P02", ago: hours(9), who: &demoPerson{"15550100010@s.whatsapp.net", "Pat Johnson"}, text: "I have some updates about the project."},
			{id: "P03", ago: hours(3), who: &demoPerson{"15550100010@s.whatsapp.net", "Pat Johnson"}, text: "Just let me know when you are around."},
			{id: "P00", ago: hours(30), text: "Morning! Notes are in the shared folder.", ack: "read"},
			{id: "P04", ago: hours(2), who: &demoPerson{"15550100010@s.whatsapp.net", "Pat Johnson"}, text: "No rush, whenever suits you."},
		}},
	}
}

// seed fills the store and the media folder with the fictional conversations.
func (d *DemoDaemon) seed(ctx context.Context, now time.Time) error {
	if err := os.MkdirAll(filepath.Join(mediaDir(d.dir), demoFilesDir), 0o700); err != nil {
		return err
	}
	for ci, c := range demoChats() {
		d.mu.Lock()
		d.pinned[c.jid], d.archived[c.jid] = c.pinned, c.archived
		d.groups[c.jid] = c.jid == demoGroupJID
		d.people[c.jid] = c.name
		for _, pp := range c.people {
			d.people[pp.jid] = pp.name
		}
		d.mu.Unlock()
		if err := d.store.SetChatName(ctx, c.jid, c.name); err != nil {
			return err
		}
		if c.avatar {
			if err := writeDemoAvatar(d.dir, c.jid, ci); err != nil {
				return err
			}
		}
		quotes := map[string]demoMsg{}
		for _, m := range c.msgs {
			quotes[m.id] = m
		}
		for _, m := range c.msgs {
			if err := d.seedMessage(ctx, c, m, quotes, now); err != nil {
				return err
			}
		}
		if err := d.store.SetUnread(ctx, c.jid, c.unread); err != nil {
			return err
		}
	}
	return nil
}

func (d *DemoDaemon) seedMessage(ctx context.Context, c demoChat, m demoMsg, quotes map[string]demoMsg, now time.Time) error {
	msg := Message{
		ID: m.id, Chat: c.jid, Text: m.text, Kind: m.kind, TS: now.Add(-m.ago).Unix(), Me: m.who == nil,
	}
	if msg.Kind == "" {
		msg.Kind = KindText
	}
	if msg.Me {
		msg.From, msg.Ack = demoSelfJID, m.ack
	} else {
		msg.From, msg.Name = m.who.jid, m.who.name
	}
	if m.quote != "" {
		q := quotes[m.quote]
		from := demoSelfJID
		if q.who != nil {
			from = q.who.jid
		}
		text, _ := clip(q.text, maxPreviewText)
		kind := q.kind
		if kind == "" {
			kind = KindText
		}
		msg.Quote = &Quote{ID: q.id, From: from, Text: text, Kind: kind, Me: q.who == nil}
	}
	if m.media != nil {
		info := *m.media
		msg.Media = &info
	}
	if m.deleted {
		// A deleted message keeps its row; the revoke below drops the content.
		msg.Text = "This message was removed."
	}
	if _, err := d.store.AddMessage(ctx, msg, false); err != nil {
		return err
	}
	if m.media != nil {
		if err := d.writeDemoMedia(ctx, msg); err != nil {
			return err
		}
	}
	for i, r := range m.reactions {
		sender := selfSender
		if r.who != nil {
			sender = r.who.jid
		}
		if _, err := d.store.ApplyReaction(ctx, c.jid, m.id, sender, r.emoji, msg.TS+int64(60*(i+1))); err != nil {
			return err
		}
	}
	if m.edited != "" {
		if _, err := d.store.ApplyEdit(ctx, c.jid, m.id, m.edited); err != nil {
			return err
		}
	}
	if m.deleted {
		if _, err := d.store.ApplyRevoke(ctx, c.jid, m.id); err != nil {
			return err
		}
	}
	return nil
}

// writeDemoMedia creates the preview and the file of a seeded media message.
func (d *DemoDaemon) writeDemoMedia(ctx context.Context, msg Message) error {
	seed := int(mediaHash(msg.Chat, msg.ID)[0])
	var file string
	var data []byte
	switch msg.Kind {
	case KindImage:
		thumb, err := encodeJPEG(demoPhoto(320, 240, seed))
		if err != nil {
			return err
		}
		if err := writeThumb(d.dir, msg.Chat, msg.ID, msg.Kind, thumb); err != nil {
			return err
		}
		if data, err = encodeJPEG(demoPhoto(800, 600, seed)); err != nil {
			return err
		}
		file = msg.ID + ".jpg"
	case KindSticker:
		var buf bytes.Buffer
		if err := png.Encode(&buf, demoSticker(256)); err != nil {
			return err
		}
		if err := writeThumb(d.dir, msg.Chat, msg.ID, msg.Kind, buf.Bytes()); err != nil {
			return err
		}
		data, file = buf.Bytes(), msg.ID+".png"
	case KindAudio:
		data, file = silentWAV(msg.Media.Seconds), msg.ID+".wav"
	case KindDocument:
		data, file = demoPDF(), msg.ID+".pdf"
	default:
		return nil
	}
	path := filepath.Join(mediaDir(d.dir), demoFilesDir, file)
	if err := writeFileAtomic(path, data); err != nil {
		return err
	}
	return d.store.SetMediaFile(ctx, msg.Chat, msg.ID, path)
}

var demoPalette = []color.RGBA{
	{0x3b, 0x82, 0xf6, 0xff}, {0xef, 0x44, 0x44, 0xff}, {0x10, 0xb9, 0x81, 0xff},
	{0xf5, 0x9e, 0x0b, 0xff}, {0x8b, 0x5c, 0xf6, 0xff}, {0xec, 0x48, 0x99, 0xff},
	{0x06, 0xb6, 0xd4, 0xff}, {0x84, 0xcc, 0x16, 0xff},
}

// writeDemoAvatar saves a round, solid-color profile picture.
func writeDemoAvatar(dir, jid string, idx int) error {
	const size = 96
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{0x24, 0x24, 0x2b, 0xff}), image.Point{}, draw.Src)
	fillCircle(img, size/2, size/2, size/2, demoPalette[idx%len(demoPalette)])
	data, err := encodeJPEG(img)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(avatarsDir(dir), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(avatarPath(dir, jid), data)
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fillCircle(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r && image.Pt(x, y).In(img.Bounds()) {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

// demoPhoto draws a vertical gradient with a sun and two hills.
func demoPhoto(w, h, seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	top, bottom := demoPalette[seed%len(demoPalette)], demoPalette[(seed+3)%len(demoPalette)]
	for y := 0; y < h; y++ {
		t := float64(y) / float64(h-1)
		c := color.RGBA{
			R: uint8(float64(top.R)*(1-t) + float64(bottom.R)*t),
			G: uint8(float64(top.G)*(1-t) + float64(bottom.G)*t),
			B: uint8(float64(top.B)*(1-t) + float64(bottom.B)*t),
			A: 0xff,
		}
		draw.Draw(img, image.Rect(0, y, w, y+1), image.NewUniform(c), image.Point{}, draw.Src)
	}
	fillCircle(img, w*3/4, h/4, h/9, color.RGBA{0xff, 0xf1, 0xa8, 0xff})
	fillCircle(img, w/4, h+h/6, h*2/3, color.RGBA{0x1f, 0x4d, 0x3a, 0xff})
	fillCircle(img, w*3/4, h+h/4, h*3/4, color.RGBA{0x2d, 0x6a, 0x4f, 0xff})
	return img
}

// demoSticker draws a round smiley on a transparent background.
func demoSticker(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	fillCircle(img, size/2, size/2, size/2-4, color.RGBA{0xfb, 0xbf, 0x24, 0xff})
	dark := color.RGBA{0x45, 0x2c, 0x0a, 0xff}
	fillCircle(img, size*3/8, size*2/5, size/16, dark)
	fillCircle(img, size*5/8, size*2/5, size/16, dark)
	for x := size * 3 / 10; x <= size*7/10; x++ {
		dx := float64(x - size/2)
		y := size*11/20 + int(dx*dx/float64(size)*0.9)
		fillCircle(img, x, y, size/40+1, dark)
	}
	return img
}

// silentWAV returns seconds of 8 kHz mono 8-bit silence.
func silentWAV(seconds int) []byte {
	const rate = 8000
	n := max(seconds, 1) * rate
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+n))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate), uint16(1), uint16(8)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(n))
	b.Write(bytes.Repeat([]byte{0x80}, n))
	return b.Bytes()
}

// demoPDF returns a tiny one-page PDF.
func demoPDF() []byte {
	stream := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", "Demo document")
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
