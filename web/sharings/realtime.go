package sharings

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

const (
	// Time allowed to write a message to the peer
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 60 * time.Second

	// Send pings to peer with this period (must be less than pongWait)
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer
	maxMessageSize = 1024
)

var upgrader = websocket.Upgrader{
	// Don't check the origin of the connexion, we check authorization later
	CheckOrigin:     func(r *http.Request) bool { return true },
	Subprotocols:    []string{"io.cozy.websocket"},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type wsResponsePayload struct {
	Type string      `json:"type"`
	ID   string      `json:"id"`
	Doc  interface{} `json:"doc,omitempty"`
}

type wsResponse struct {
	Event   string            `json:"event"`
	Payload wsResponsePayload `json:"payload"`
}

type wsErrorPayload struct {
	Status string      `json:"status"`
	Code   string      `json:"code"`
	Title  string      `json:"title"`
	Source interface{} `json:"source"`
}

type wsError struct {
	Event   string         `json:"event"`
	Payload wsErrorPayload `json:"payload"`
}

func unauthorized(cmd interface{}) *wsError {
	return &wsError{
		Event: "error",
		Payload: wsErrorPayload{
			Status: "401 Unauthorized",
			Code:   "unauthorized",
			Title:  "The authentication has failed",
			Source: cmd,
		},
	}
}

func unknownMethod(method string, cmd interface{}) *wsError {
	return &wsError{
		Event: "error",
		Payload: wsErrorPayload{
			Status: "405 Method Not Allowed",
			Code:   "method not allowed",
			Title:  fmt.Sprintf("The %s method is not supported", method),
			Source: cmd,
		},
	}
}

func sendErr(ctx context.Context, errc chan *wsError, e *wsError) {
	select {
	case errc <- e:
	case <-ctx.Done():
	}
}

func upgradeWs(c echo.Context) (*websocket.Conn, error) {
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxMessageSize)
	if err = ws.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return nil, errors.New("SetReadDeadline")
	}
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	return ws, nil
}

func getPermission(c echo.Context, i *instance.Instance, ws *websocket.Conn) (*permission.Permission, *wsError) {
	var auth map[string]string
	if err := ws.ReadJSON(&auth); err != nil {
		return nil, unknownMethod(auth["method"], auth)
	}
	if strings.ToUpper(auth["method"]) != "AUTH" {
		return nil, unknownMethod(auth["method"], auth)
	}
	if auth["payload"] == "" {
		return nil, unauthorized(auth)
	}
	pdoc, err := middlewares.ParseJWT(c, i, auth["payload"])
	if err != nil {
		return nil, unauthorized(auth)
	}
	return pdoc, nil
}

func wsWrite(ws *websocket.Conn, ch chan *wsResponse, errc chan *wsError) error {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case e, ok := <-errc:
			if !ok { // Websocket has been closed by the client
				return nil
			}
			if err := ws.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return nil
			}
			if err := ws.WriteJSON(e); err != nil {
				return nil
			}
		case res := <-ch:
			if err := ws.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return nil
			}
			if err := ws.WriteJSON(res); err != nil {
				return nil
			}
		case <-ticker.C:
			if err := ws.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return err
			}
			if err := ws.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				return nil
			}
		}
	}
}

func filterMapEvents(ctx context.Context, ds *realtime.Subscriber, ch chan *wsResponse, inst *instance.Instance, s *sharing.Sharing) {
	log := inst.Logger().WithNamespace("sharing-realtime")

	if s.HasFileDriveRoot() {
		rootFileID, err := s.DriveRootID()
		if err != nil {
			log.Errorf("filterMapEvents: missing root file for sharing %s", s.SID)
			return
		}
		log.Debugf("filterMapEvents: watching file %s for sharing %s", rootFileID, s.SID)

		isRoot := func(doc realtime.Doc) bool {
			return doc != nil && doc.ID() == rootFileID
		}
		forwardEvents(ctx, ds, ch, func(e *realtime.Event) bool {
			return isRoot(e.Doc) || isRoot(e.OldDoc)
		})
		return
	}

	var dir vfs.DirDoc
	if err := couchdb.GetDoc(inst, consts.Files, s.Rules[0].Values[0], &dir); err != nil {
		log.Errorf("filterMapEvents: failed to get root dir %s: %s", s.Rules[0].Values[0], err)
		return
	}
	log.Debugf("filterMapEvents: watching %s for sharing %s", dir.Fullpath, s.SID)

	testPath := func(doc realtime.Doc) bool {
		if d, ok := doc.(*vfs.DirDoc); ok {
			return strings.HasPrefix(d.Fullpath, dir.Fullpath+"/")
		}
		if f, ok := doc.(*vfs.FileDoc); ok {
			if f.Trashed {
				return strings.HasPrefix(f.RestorePath, dir.Fullpath+"/")
			}
			p, err := f.Path(inst.VFS())
			if err != nil {
				return false
			}
			return strings.HasPrefix(p, dir.Fullpath+"/")
		}
		// Handle realtime.JSONDoc (happens when events come through Redis in multi-stack)
		if j, ok := doc.(*realtime.JSONDoc); ok {
			docType, _ := j.M["type"].(string)
			if docType == consts.DirType {
				fullpath, _ := j.M["path"].(string)
				return strings.HasPrefix(fullpath, dir.Fullpath+"/")
			}
			dirID, _ := j.M["dir_id"].(string)
			if dirID == "" {
				log.Warnf("filterMapEvents: failed to get dir_id in event doc with id:%s", doc.ID())
				return false
			}
			parentDir, err := inst.VFS().DirByID(dirID)
			if err != nil {
				log.Warnf("filterMapEvents: failed to get parent dir %s: %s", dirID, err)
				return false
			}
			name, _ := j.M["name"].(string)
			filePath := parentDir.Fullpath + "/" + name
			return strings.HasPrefix(filePath, dir.Fullpath+"/")
		}
		return false
	}

	match := func(e *realtime.Event) bool {
		if e.Doc.ID() == dir.DocID {
			return true
		}
		if e.OldDoc != nil && e.OldDoc.ID() == dir.DocID {
			return true
		}
		if testPath(e.Doc) {
			return true
		}
		if e.OldDoc != nil && testPath(e.OldDoc) {
			return true
		}
		return false
	}

	forwardEvents(ctx, ds, ch, match)
}

// forwardEvents sends the matching events of ds to ch until ctx is done. The
// websocket writer stops reading ch when the client disconnects, so every
// send must also watch ctx.
func forwardEvents(ctx context.Context, ds *realtime.Subscriber, ch chan *wsResponse, match func(*realtime.Event) bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ds.Channel:
			if !match(e) {
				continue
			}
			res := &wsResponse{
				Event: e.Verb,
				Payload: wsResponsePayload{
					Type: e.Doc.DocType(),
					ID:   e.Doc.ID(),
					Doc:  e.Doc,
				},
			}
			select {
			case ch <- res:
			case <-ctx.Done():
				return
			}
		}
	}
}

func wsOwner(c echo.Context, inst *instance.Instance, s *sharing.Sharing) error {
	log := inst.Logger().WithNamespace("sharing-realtime")

	ws, err := upgradeWs(c)
	if err != nil {
		return nil
	}
	defer ws.Close()

	ds := realtime.GetHub().Subscriber(inst)
	defer ds.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan *wsError)
	ch := make(chan *wsResponse)

	go func() {
		defer close(ch)
		defer close(errc)
		pdoc, wsErr := getPermission(c, inst, ws)
		if wsErr != nil {
			log.Warnf("wsOwner: AUTH failed for sharing %s", s.SID)
			sendErr(ctx, errc, wsErr)
			return
		}
		for _, rule := range s.Rules {
			for _, value := range rule.Values {
				if !pdoc.Permissions.AllowID(permission.GET, rule.DocType, value) {
					log.Warnf("wsOwner: permission denied for sharing %s", s.SID)
					sendErr(ctx, errc, unauthorized(pdoc))
					return
				}
			}
		}
		ds.Subscribe(consts.Files)
		filterMapEvents(ctx, ds, ch, inst, s)
	}()

	return wsWrite(ws, ch, errc)
}

// Ws is the API handler for realtime via a websocket connection.
func Ws(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	log := inst.Logger().WithNamespace("sharing-realtime")

	s, err := sharing.FindSharing(inst, c.Param("id"))
	if err != nil {
		return wrapErrors(err)
	}
	if !s.Drive {
		return jsonapi.NotFound(errors.New("not a drive"))
	}

	if s.Owner {
		log.Debugf("Ws: new connection for sharing %s (owner mode)", s.SID)
		return wsOwner(c, inst, s)
	}

	if len(s.Credentials) == 0 {
		return jsonapi.InternalServerError(errors.New("no credentials"))
	}
	token := s.Credentials[0].DriveToken
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return jsonapi.InternalServerError(err)
	}

	// Background consumers (like the dataproxy indexer) watch the drive
	// without the user looking at it, so they must not consume the "new"
	// status that feeds the sharings news counter.
	if c.QueryParam("background") != "true" {
		go func() {
			if err := markSharingAsSeen(inst, s); err != nil {
				inst.Logger().WithNamespace("sharing").
					Warnf("markSharingAsSeen failed: %s", err)
			}
		}()
	}

	// XXX Let's try to avoid one http request by cheating a bit. If the two
	// instances are on the same domain (same stack), we can watch directly
	// the real-time events. It helps for performances.
	if owner, err := lifecycle.GetInstance(u.Host); err == nil {
		log.Debugf("Ws: new connection for sharing %s (hijack mode, owner=%s)", s.SID, owner.Domain)
		return wsHijack(c, inst, owner, s)
	}
	log.Debugf("Ws: new connection for sharing %s (proxy mode, owner=%s)", s.SID, u.Host)
	return wsProxy(c, inst, s, token)
}

func wsHijack(c echo.Context, inst, owner *instance.Instance, s *sharing.Sharing) error {
	log := inst.Logger().WithNamespace("sharing-realtime")

	ws, err := upgradeWs(c)
	if err != nil {
		return nil
	}
	defer ws.Close()

	ds := realtime.GetHub().Subscriber(owner)
	defer ds.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan *wsError)
	ch := make(chan *wsResponse)

	go func() {
		defer close(errc)
		pdoc, wsErr := getPermission(c, inst, ws)
		if wsErr != nil {
			log.Warnf("wsHijack: AUTH failed for sharing %s", s.SID)
			sendErr(ctx, errc, wsErr)
			return
		}
		if !pdoc.Permissions.AllowWholeType(permission.GET, consts.Files) {
			log.Warnf("wsHijack: permission denied for sharing %s", s.SID)
			sendErr(ctx, errc, unauthorized(pdoc))
			return
		}
		ds.Subscribe(consts.Files)
		filterMapEvents(ctx, ds, ch, owner, s)
	}()

	return wsWrite(ws, ch, errc)
}

func wsProxy(c echo.Context, inst *instance.Instance, s *sharing.Sharing, token string) error {
	log := inst.Logger().WithNamespace("sharing-realtime")

	ws, err := upgradeWs(c)
	if err != nil {
		return nil
	}
	defer ws.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan *wsError)
	ch := make(chan *wsResponse)

	go func() {
		defer close(ch)
		defer close(errc)
		pdoc, wsErr := getPermission(c, inst, ws)
		if wsErr != nil {
			log.Warnf("wsProxy: AUTH failed for sharing %s", s.SID)
			sendErr(ctx, errc, wsErr)
			return
		}
		if !pdoc.Permissions.AllowWholeType(permission.GET, consts.Files) {
			log.Warnf("wsProxy: permission denied for sharing %s", s.SID)
			sendErr(ctx, errc, unauthorized(pdoc))
			return
		}

		u, err := url.Parse(s.Members[0].Instance)
		if err != nil {
			return
		}
		var scheme string
		if u.Scheme == "https" {
			scheme = "wss"
		} else {
			scheme = "ws"
		}
		wsURL := url.URL{
			Scheme: scheme,
			Host:   u.Host,
			Path:   fmt.Sprintf("/sharings/drives/%s/realtime", s.SID),
		}

		ownerWS, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
		if err != nil {
			log.Errorf("wsProxy: failed to dial owner %s: %s", wsURL.String(), err)
			return
		}
		defer ownerWS.Close()

		authMsg := map[string]string{
			"method":  "AUTH",
			"payload": token,
		}
		if err := ownerWS.WriteJSON(authMsg); err != nil {
			log.Errorf("wsProxy: failed to send AUTH to owner: %s", err)
			return
		}
		log.Debugf("wsProxy: connected to owner %s for sharing %s", u.Host, s.SID)

		for {
			var msg wsResponse
			if err := ownerWS.ReadJSON(&msg); err != nil {
				return
			}
			select {
			case ch <- &msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	return wsWrite(ws, ch, errc)
}

func markSharingAsSeen(inst *instance.Instance, s *sharing.Sharing) error {
	key := []string{consts.Sharings, s.SID}
	end := []string{key[0], key[1], couchdb.MaxString}
	req := &couchdb.ViewRequest{
		StartKey:    key,
		EndKey:      end,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, req, &res)
	if err != nil {
		return err
	}
	if len(res.Rows) == 0 {
		return nil
	}
	fs := inst.VFS()
	file, err := fs.FileByID(res.Rows[0].ID)
	if err != nil {
		return err
	}

	if meta, ok := file.Metadata["sharing"].(map[string]interface{}); ok && meta["status"] != "seen" {
		old := file.Clone().(*vfs.FileDoc)
		meta["status"] = "seen"
		return fs.UpdateFileDoc(old, file)
	}
	return nil
}
