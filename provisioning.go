// mautrix-whatsapp - A Matrix-WhatsApp puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"go.mau.fi/util/requestlog"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"github.com/element-hq/mautrix-go/bridge/status"
	"github.com/element-hq/mautrix-go/id"
)

type ProvisioningAPI struct {
	bridge *WABridge
	log    zerolog.Logger
}

func (prov *ProvisioningAPI) Init() {
	prov.log.Debug().Str("base_path", prov.bridge.Config.Bridge.Provisioning.Prefix).Msg("Enabling provisioning API")
	r := prov.bridge.AS.Router.PathPrefix(prov.bridge.Config.Bridge.Provisioning.Prefix).Subrouter()
	r.Use(hlog.NewHandler(prov.log))
	r.Use(requestlog.AccessLogger(true))
	r.Use(prov.AuthMiddleware)
	r.HandleFunc("/v1/ping", prov.Ping).Methods(http.MethodGet)
	r.HandleFunc("/v1/login", prov.Login).Methods(http.MethodGet)
	r.HandleFunc("/v1/logout", prov.Logout).Methods(http.MethodPost)
	r.HandleFunc("/v1/delete_session", prov.DeleteSession).Methods(http.MethodPost)
	r.HandleFunc("/v1/disconnect", prov.Disconnect).Methods(http.MethodPost)
	r.HandleFunc("/v1/reconnect", prov.Reconnect).Methods(http.MethodPost)
	r.HandleFunc("/v1/debug/appstate/{name}", prov.SyncAppState).Methods(http.MethodPost)
	r.HandleFunc("/v1/contacts", prov.ListContacts).Methods(http.MethodGet)
	r.HandleFunc("/v1/groups", prov.ListGroups).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc("/v1/resolve_identifier/{number}", prov.ResolveIdentifier).Methods(http.MethodGet)
	r.HandleFunc("/v1/bulk_resolve_identifier", prov.BulkResolveIdentifier).Methods(http.MethodPost)
	r.HandleFunc("/v1/pm/{number}", prov.StartPM).Methods(http.MethodPost)
	r.HandleFunc("/v1/open/{groupID}", prov.OpenGroup).Methods(http.MethodPost)
	r.HandleFunc("/v1/group/open/{groupID}", prov.OpenGroup).Methods(http.MethodPost)
	r.HandleFunc("/v1/group/resolve/{inviteCode}", prov.ResolveGroupInvite).Methods(http.MethodPost)
	r.HandleFunc("/v1/group/join/{inviteCode}", prov.JoinGroup).Methods(http.MethodPost)
	prov.bridge.AS.Router.HandleFunc("/_matrix/app/com.beeper.asmux/ping", prov.BridgeStatePing).Methods(http.MethodPost)
	prov.bridge.AS.Router.HandleFunc("/_matrix/app/com.beeper.bridge_state", prov.BridgeStatePing).Methods(http.MethodPost)

	if prov.bridge.Config.Bridge.Provisioning.DebugEndpoints {
		prov.log.Debug().Msg("Enabling debug API at /debug")
		r := prov.bridge.AS.Router.PathPrefix("/debug").Subrouter()
		r.Use(prov.AuthMiddleware)
		r.PathPrefix("/pprof").Handler(http.DefaultServeMux)
	}

	// Deprecated, just use /disconnect
	r.HandleFunc("/v1/delete_connection", prov.Disconnect).Methods(http.MethodPost)
}

func (prov *ProvisioningAPI) AuthMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if len(auth) == 0 && strings.HasSuffix(r.URL.Path, "/login") {
			authParts := strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
			for _, part := range authParts {
				part = strings.TrimSpace(part)
				if strings.HasPrefix(part, "net.maunium.whatsapp.auth-") {
					auth = part[len("net.maunium.whatsapp.auth-"):]
					break
				}
			}
		} else if strings.HasPrefix(auth, "Bearer ") {
			auth = auth[len("Bearer "):]
		}
		if auth != prov.bridge.Config.Bridge.Provisioning.SharedSecret {
			hlog.FromRequest(r).Debug().Msg("Authentication token does not match shared secret")
			jsonResponse(w, http.StatusForbidden, map[string]interface{}{
				"error":   "Authentication token does not match shared secret",
				"errcode": "M_FORBIDDEN",
			})
			return
		}
		userID := r.URL.Query().Get("user_id")
		user := prov.bridge.GetUserByMXID(id.UserID(userID))
		if user != nil {
			hlog.FromRequest(r).UpdateContext(func(c zerolog.Context) zerolog.Context {
				return c.Stringer("user_id", user.MXID)
			})
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), "user", user)))
	})
}

type Error struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	ErrCode string `json:"errcode"`
}

type Response struct {
	Success bool   `json:"success"`
	Status  string `json:"status"`
}

func (prov *ProvisioningAPI) DeleteSession(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	if user.Session == nil && user.Client == nil {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   "Nothing to purge: no session information stored and no active connection.",
			ErrCode: "no session",
		})
		return
	}
	user.DeleteConnection()
	user.DeleteSession(r.Context())
	jsonResponse(w, http.StatusOK, Response{true, "Session information purged"})
	user.removeFromJIDMap(status.BridgeState{StateEvent: status.StateLoggedOut})
}

func (prov *ProvisioningAPI) Disconnect(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	if user.Client == nil {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   "You don't have a WhatsApp connection.",
			ErrCode: "no connection",
		})
		return
	}
	user.DeleteConnection()
	jsonResponse(w, http.StatusOK, Response{true, "Disconnected from WhatsApp"})
	user.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: WANotConnected})
}

func (prov *ProvisioningAPI) Reconnect(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	if user.Client == nil {
		if user.Session == nil {
			jsonResponse(w, http.StatusForbidden, Error{
				Error:   "No existing connection and no session. Please log in first.",
				ErrCode: "no session",
			})
		} else {
			user.Connect()
			jsonResponse(w, http.StatusAccepted, Response{true, "Created connection to WhatsApp."})
		}
	} else {
		user.DeleteConnection()
		user.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: WANotConnected})
		user.Connect()
		jsonResponse(w, http.StatusAccepted, Response{true, "Restarted connection to WhatsApp"})
	}
}

func (prov *ProvisioningAPI) SyncAppState(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	if user == nil || user.Client == nil {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   "User is not connected to WhatsApp",
			ErrCode: "no session",
		})
		return
	}

	vars := mux.Vars(r)
	nameStr := vars["name"]
	if len(nameStr) == 0 {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "The `name` parameter is required",
			ErrCode: "missing-name-param",
		})
		return
	}
	var name appstate.WAPatchName
	for _, existingName := range appstate.AllPatchNames {
		if nameStr == string(existingName) {
			name = existingName
		}
	}
	if len(name) == 0 {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   fmt.Sprintf("'%s' is not a valid app state patch name", nameStr),
			ErrCode: "invalid-name-param",
		})
		return
	}
	fullStr := r.URL.Query().Get("full")
	fullSync := len(fullStr) > 0 && (fullStr == "1" || strings.ToLower(fullStr)[0] == 't')
	err := user.Client.FetchAppState(name, fullSync, false)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, Error{false, err.Error(), "sync-fail"})
	} else {
		jsonResponse(w, http.StatusOK, Response{true, fmt.Sprintf("Synced app state %s", name)})
	}
}

func (prov *ProvisioningAPI) ListContacts(w http.ResponseWriter, r *http.Request) {
	if user := r.Context().Value("user").(*User); user.Session == nil {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "User is not logged into WhatsApp",
			ErrCode: "no session",
		})
	} else if contacts, err := user.Session.Contacts.GetAllContacts(); err != nil {
		hlog.FromRequest(r).Err(err).Msg("Failed to fetch all contacts")
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   "Internal server error while fetching contact list",
			ErrCode: "failed to get contacts",
		})
	} else {
		augmentedContacts := map[types.JID]interface{}{}
		for jid, contact := range contacts {
			var avatarUrl id.ContentURI
			if puppet := prov.bridge.GetPuppetByJID(jid); puppet != nil {
				avatarUrl = puppet.AvatarURL
			}
			augmentedContacts[jid] = map[string]interface{}{
				"Found":        contact.Found,
				"FirstName":    contact.FirstName,
				"FullName":     contact.FullName,
				"PushName":     contact.PushName,
				"BusinessName": contact.BusinessName,
				"AvatarURL":    avatarUrl,
			}
		}
		jsonResponse(w, http.StatusOK, augmentedContacts)
	}
}

func (prov *ProvisioningAPI) ListGroups(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	if user.Session == nil {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "User is not logged into WhatsApp",
			ErrCode: "no session",
		})
		return
	}
	if r.Method == http.MethodPost {
		err := user.ResyncGroups(r.URL.Query().Get("create_portals") == "true")
		if err != nil {
			hlog.FromRequest(r).Err(err).Msg("Failed to resync groups")
			jsonResponse(w, http.StatusInternalServerError, Error{
				Error:   "Internal server error while resyncing groups",
				ErrCode: "failed to sync groups",
			})
			return
		}
	}
	if groups, err := user.getCachedGroupList(); err != nil {
		hlog.FromRequest(r).Err(err).Msg("Failed to fetch group list")
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   "Internal server error while fetching group list",
			ErrCode: "failed to get groups",
		})
	} else {
		jsonResponse(w, http.StatusOK, groups)
	}
}

type OtherUserInfo struct {
	MXID   id.UserID     `json:"mxid"`
	JID    types.JID     `json:"jid"`
	Name   string        `json:"displayname"`
	Avatar id.ContentURI `json:"avatar_url"`
}

type PortalInfo struct {
	RoomID      id.RoomID        `json:"room_id"`
	OtherUser   *OtherUserInfo   `json:"other_user,omitempty"`
	GroupInfo   *types.GroupInfo `json:"group_info,omitempty"`
	JustCreated bool             `json:"just_created"`
}

func looksEmaily(str string) bool {
	for _, char := range str {
		// Characters that are usually in emails, but shouldn't be in phone numbers
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '@' {
			return true
		}
	}
	return false
}

func (prov *ProvisioningAPI) resolveIdentifier(w http.ResponseWriter, r *http.Request) (types.JID, *User) {
	number, _ := mux.Vars(r)["number"]
	if strings.HasSuffix(number, "@"+types.DefaultUserServer) {
		jid, _ := types.ParseJID(number)
		number = "+" + jid.User
	}
	if looksEmaily(number) {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "WhatsApp only supports phone numbers as user identifiers",
			ErrCode: "number looks like email",
		})
	} else if user := r.Context().Value("user").(*User); !user.IsLoggedIn() {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "User is not logged into WhatsApp",
			ErrCode: "no session",
		})
	} else if resp, err := user.Client.IsOnWhatsApp([]string{number}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   fmt.Sprintf("Failed to check if number is on WhatsApp: %v", err),
			ErrCode: "error checking number",
		})
	} else if len(resp) == 0 {
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   "Didn't get a response to checking if the number is on WhatsApp",
			ErrCode: "error checking number",
		})
	} else if !resp[0].IsIn {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   fmt.Sprintf("The server said +%s is not on WhatsApp", resp[0].JID.User),
			ErrCode: "not on whatsapp",
		})
	} else {
		return resp[0].JID, user
	}
	return types.EmptyJID, nil
}

func (prov *ProvisioningAPI) StartPM(w http.ResponseWriter, r *http.Request) {
	jid, user := prov.resolveIdentifier(w, r)
	if jid.IsEmpty() || user == nil {
		// resolveIdentifier already responded with an error
		return
	}
	portal, puppet, justCreated, err := user.StartPM(r.Context(), jid, "provisioning API PM")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error: fmt.Sprintf("Failed to create portal: %v", err),
		})
	}
	statusCode := http.StatusOK
	if justCreated {
		statusCode = http.StatusCreated
	}
	jsonResponse(w, statusCode, PortalInfo{
		RoomID: portal.MXID,
		OtherUser: &OtherUserInfo{
			JID:    puppet.JID,
			MXID:   puppet.MXID,
			Name:   puppet.Displayname,
			Avatar: puppet.AvatarURL,
		},
		JustCreated: justCreated,
	})
}

func (prov *ProvisioningAPI) ResolveIdentifier(w http.ResponseWriter, r *http.Request) {
	jid, user := prov.resolveIdentifier(w, r)
	if jid.IsEmpty() || user == nil {
		// resolveIdentifier already responded with an error
		return
	}
	portal := user.GetPortalByJID(jid)
	puppet := user.bridge.GetPuppetByJID(jid)
	jsonResponse(w, http.StatusOK, PortalInfo{
		RoomID: portal.MXID,
		OtherUser: &OtherUserInfo{
			JID:    puppet.JID,
			MXID:   puppet.MXID,
			Name:   puppet.Displayname,
			Avatar: puppet.AvatarURL,
		},
	})
}

type ReqBulkResolveIdentifier struct {
	Numbers []string `json:"numbers"`
}

func (prov *ProvisioningAPI) BulkResolveIdentifier(w http.ResponseWriter, r *http.Request) {
	var req ReqBulkResolveIdentifier
	var resp []types.IsOnWhatsAppResponse
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "Failed to parse request JSON",
			ErrCode: "bad json",
		})
	} else if user := r.Context().Value("user").(*User); !user.IsLoggedIn() {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "User is not logged into WhatsApp",
			ErrCode: "no session",
		})
	} else if resp, err = user.Client.IsOnWhatsApp(req.Numbers); err != nil {
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   fmt.Sprintf("Failed to check if number is on WhatsApp: %v", err),
			ErrCode: "error checking number",
		})
	} else {
		jsonResponse(w, http.StatusOK, resp)
	}
}

func (prov *ProvisioningAPI) OpenGroup(w http.ResponseWriter, r *http.Request) {
	groupID, _ := mux.Vars(r)["groupID"]
	if user := r.Context().Value("user").(*User); !user.IsLoggedIn() {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "User is not logged into WhatsApp",
			ErrCode: "no session",
		})
	} else if jid, err := types.ParseJID(groupID); err != nil || jid.Server != types.GroupServer || (!strings.ContainsRune(jid.User, '-') && len(jid.User) < 15) {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "Invalid group ID",
			ErrCode: "invalid group id",
		})
	} else if info, err := user.Client.GetGroupInfo(jid); err != nil {
		hlog.FromRequest(r).Err(err).Msg("Failed to get group info by JID")
		// TODO return better responses for different errors (like ErrGroupNotFound and ErrNotInGroup)
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   fmt.Sprintf("Failed to get group info: %v", err),
			ErrCode: "error getting group info",
		})
	} else {
		hlog.FromRequest(r).Debug().Stringer("chat_jid", jid).Msg("Importing group chat for user")
		portal := user.GetPortalByJID(info.JID)
		statusCode := http.StatusOK
		if len(portal.MXID) == 0 {
			err = portal.CreateMatrixRoom(r.Context(), user, info, nil, true, true)
			if err != nil {
				jsonResponse(w, http.StatusInternalServerError, Error{
					Error: fmt.Sprintf("Failed to create portal: %v", err),
				})
				return
			}
			statusCode = http.StatusCreated
		}
		jsonResponse(w, statusCode, PortalInfo{
			RoomID:      portal.MXID,
			GroupInfo:   info,
			JustCreated: statusCode == http.StatusCreated,
		})
	}
}

func (prov *ProvisioningAPI) resolveGroupInvite(w http.ResponseWriter, r *http.Request) (*types.GroupInfo, *User) {
	inviteCode, _ := mux.Vars(r)["inviteCode"]
	if user := r.Context().Value("user").(*User); !user.IsLoggedIn() {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "User is not logged into WhatsApp",
			ErrCode: "no session",
		})
	} else if info, err := user.Client.GetGroupInfoFromLink(inviteCode); err != nil {
		if errors.Is(err, whatsmeow.ErrInviteLinkRevoked) {
			jsonResponse(w, http.StatusBadRequest, Error{
				Error:   whatsmeow.ErrInviteLinkRevoked.Error(),
				ErrCode: "invite link revoked",
			})
		} else if errors.Is(err, whatsmeow.ErrInviteLinkInvalid) {
			jsonResponse(w, http.StatusBadRequest, Error{
				Error:   whatsmeow.ErrInviteLinkInvalid.Error(),
				ErrCode: "invalid invite link",
			})
		} else {
			hlog.FromRequest(r).Err(err).Msg("Failed to get group info from link")
			jsonResponse(w, http.StatusInternalServerError, Error{
				Error:   fmt.Sprintf("Failed to fetch group info with link: %v", err),
				ErrCode: "error getting group info",
			})
		}
	} else {
		return info, user
	}
	return nil, nil
}

func (prov *ProvisioningAPI) ResolveGroupInvite(w http.ResponseWriter, r *http.Request) {
	info, user := prov.resolveGroupInvite(w, r)
	if info == nil {
		return
	}
	jsonResponse(w, http.StatusOK, PortalInfo{
		RoomID:    user.GetPortalByJID(info.JID).MXID,
		GroupInfo: info,
	})
}

func (prov *ProvisioningAPI) JoinGroup(w http.ResponseWriter, r *http.Request) {
	info, user := prov.resolveGroupInvite(w, r)
	if info == nil {
		return
	}
	user.groupJoinLock.Lock()
	user.skipGroupCreateDelay = info.JID
	defer func() {
		user.skipGroupCreateDelay = types.EmptyJID
		user.groupJoinLock.Unlock()
	}()
	inviteCode, _ := mux.Vars(r)["inviteCode"]
	if jid, err := user.Client.JoinGroupWithLink(inviteCode); err != nil {
		hlog.FromRequest(r).Err(err).Msg("Failed to join group")
		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   fmt.Sprintf("Failed to join group: %v", err),
			ErrCode: "error joining group",
		})
	} else {
		hlog.FromRequest(r).Debug().Stringer("chat_jid", jid).Msg("Successfully joined group")
		portal := user.GetPortalByJID(jid)
		statusCode := http.StatusOK
		if len(portal.MXID) == 0 {
			time.Sleep(500 * time.Millisecond) // Wait for incoming group info to create the portal automatically
			err = portal.CreateMatrixRoom(r.Context(), user, info, nil, true, true)
			if err != nil {
				jsonResponse(w, http.StatusInternalServerError, Error{
					Error: fmt.Sprintf("Failed to create portal: %v", err),
				})
				return
			}
			statusCode = http.StatusCreated
		}
		jsonResponse(w, statusCode, PortalInfo{
			RoomID:      portal.MXID,
			GroupInfo:   info,
			JustCreated: statusCode == http.StatusCreated,
		})
	}
}

func (prov *ProvisioningAPI) Ping(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	wa := map[string]interface{}{
		"has_session":     user.Session != nil,
		"management_room": user.ManagementRoom,
		"conn":            nil,
	}
	if !user.JID.IsEmpty() {
		wa["jid"] = user.JID.String()
		wa["phone"] = "+" + user.JID.User
		wa["device"] = user.JID.Device
		if user.Session != nil {
			wa["platform"] = user.Session.Platform
		}
	}
	if user.Client != nil {
		wa["conn"] = map[string]interface{}{
			"is_connected": user.Client.IsConnected(),
			"is_logged_in": user.Client.IsLoggedIn(),
		}
	}
	resp := map[string]interface{}{
		"mxid":              user.MXID,
		"admin":             user.Admin,
		"whitelisted":       user.Whitelisted,
		"relay_whitelisted": user.RelayWhitelisted,
		"whatsapp":          wa,
	}
	jsonResponse(w, http.StatusOK, resp)
}

func jsonResponse(w http.ResponseWriter, status int, response interface{}) {
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func (prov *ProvisioningAPI) Logout(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)
	if user.Session == nil {
		jsonResponse(w, http.StatusOK, Error{
			Error:   "You're not logged in",
			ErrCode: "not logged in",
		})
		return
	}

	force := strings.ToLower(r.URL.Query().Get("force")) != "false"

	if user.Client == nil {
		if !force {
			jsonResponse(w, http.StatusNotFound, Error{
				Error:   "You're not connected",
				ErrCode: "not connected",
			})
		}
	} else {
		err := user.Client.Logout()
		if err != nil {
			hlog.FromRequest(r).Err(err).Msg("Unknown error while logging out")
			if !force {
				jsonResponse(w, http.StatusInternalServerError, Error{
					Error:   fmt.Sprintf("Unknown error while logging out: %v", err),
					ErrCode: err.Error(),
				})
				return
			}
		} else {
			user.Session = nil
		}
		user.DeleteConnection()
	}

	user.bridge.Metrics.TrackConnectionState(user.JID, false)
	user.removeFromJIDMap(status.BridgeState{StateEvent: status.StateLoggedOut})
	user.DeleteSession(r.Context())
	jsonResponse(w, http.StatusOK, Response{true, "Logged out successfully."})
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	Subprotocols: []string{"net.maunium.whatsapp.login"},
}

func (prov *ProvisioningAPI) Login(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	user := prov.bridge.GetUserByMXID(id.UserID(userID))
	log := hlog.FromRequest(r)

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Err(err).Msg("Failed to upgrade connection to websocket")
		return
	}
	defer func() {
		err := c.Close()
		if err != nil {
			log.Debug().Err(err).Msg("Error closing websocket")
		}
	}()

	go func() {
		// Read everything so SetCloseHandler() works
		for {
			_, _, err = c.ReadMessage()
			if err != nil {
				break
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	c.SetCloseHandler(func(code int, text string) error {
		log.Debug().Int("close_code", code).Msg("Login websocket closed, cancelling login")
		cancel()
		return nil
	})

	if userTimezone := r.URL.Query().Get("tz"); userTimezone != "" {
		log.Debug().Str("timezone", userTimezone).Msg("Updating user timezone")
		user.Timezone = userTimezone
		err = user.Update(r.Context())
		if err != nil {
			log.Err(err).Msg("Failed to save user after updating timezone")
		}
	} else {
		log.Debug().Msg("No timezone provided in request")
	}

	qrChan, err := user.Login(ctx)
	expiryTime := time.Now().Add(160 * time.Second)
	if err != nil {
		log.Err(err).Msg("Failed to log in via provisioning API")
		if errors.Is(err, ErrAlreadyLoggedIn) {
			go user.Connect()
			_ = c.WriteJSON(Error{
				Error:   "You're already logged into WhatsApp",
				ErrCode: "already logged in",
			})
		} else {
			_ = c.WriteJSON(Error{
				Error:   "Failed to connect to WhatsApp",
				ErrCode: "connection error",
			})
		}
	}
	phoneNum := r.URL.Query().Get("phone_number")
	if phoneNum != "" {
		pairingCode, err := user.Client.PairPhone(phoneNum, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			log.Err(err).Msg("Failed to start phone code login")
			_ = c.WriteJSON(Error{
				Error:   "Failed to request pairing code",
				ErrCode: "code error",
			})
			go user.DeleteConnection()
			return
		} else {
			log.Debug().Msg("Started phone number login")
			_ = c.WriteJSON(map[string]any{
				"pairing_code": pairingCode,
				"timeout":      int(time.Until(expiryTime).Seconds()),
			})
		}
	}

	log.Debug().Msg("Started login via provisioning API")
	Analytics.Track(user.MXID, "$login_start")

	for {
		select {
		case evt := <-qrChan:
			switch evt.Event {
			case whatsmeow.QRChannelSuccess.Event:
				jid := user.Client.Store.ID
				log.Debug().Stringer("jid", jid).Msg("Successful login via provisioning API")
				Analytics.Track(user.MXID, "$login_success")
				_ = c.WriteJSON(map[string]interface{}{
					"success":  true,
					"jid":      jid,
					"phone":    fmt.Sprintf("+%s", jid.User),
					"platform": user.Client.Store.Platform,
				})
			case whatsmeow.QRChannelTimeout.Event:
				log.Debug().Msg("Login via provisioning API timed out")
				errCode := "login timed out"
				Analytics.Track(user.MXID, "$login_failure", map[string]interface{}{"error": errCode})
				_ = c.WriteJSON(Error{
					Error:   "QR code scan timed out. Please try again.",
					ErrCode: errCode,
				})
			case whatsmeow.QRChannelErrUnexpectedEvent.Event:
				log.Debug().Msg("Login via provisioning API failed due to unexpected event")
				errCode := "unexpected event"
				Analytics.Track(user.MXID, "$login_failure", map[string]interface{}{"error": errCode})
				_ = c.WriteJSON(Error{
					Error:   "Got unexpected event while waiting for QRs, perhaps you're already logged in?",
					ErrCode: errCode,
				})
			case whatsmeow.QRChannelClientOutdated.Event:
				log.Debug().Msg("Login via provisioning API failed due to outdated client")
				errCode := "bridge outdated"
				Analytics.Track(user.MXID, "$login_failure", map[string]interface{}{"error": errCode})
				_ = c.WriteJSON(Error{
					Error:   "Got client outdated error while waiting for QRs. The bridge must be updated to continue.",
					ErrCode: errCode,
				})
			case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
				errCode := "multidevice not enabled"
				Analytics.Track(user.MXID, "$login_failure", map[string]interface{}{"error": errCode})
				_ = c.WriteJSON(Error{
					Error:   "Please enable the WhatsApp multidevice beta and scan the QR code again.",
					ErrCode: errCode,
				})
				continue
			case "error":
				errCode := "fatal error"
				Analytics.Track(user.MXID, "$login_failure", map[string]interface{}{"error": errCode})
				_ = c.WriteJSON(Error{
					Error:   "Fatal error while logging in",
					ErrCode: errCode,
				})
			case "code":
				Analytics.Track(user.MXID, "$qrcode_retrieved")
				_ = c.WriteJSON(map[string]interface{}{
					"code":    evt.Code,
					"timeout": int(evt.Timeout.Seconds()),
				})
				continue
			}
			return
		case <-ctx.Done():
			return
		}
	}
}
