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
	_ "embed"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"

	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util/configupgrade"

	"maunium.net/go/mautrix-whatsapp/config"
	"maunium.net/go/mautrix-whatsapp/database"
)

const ONE_DAY_S = 24 * 60 * 60

// Information to find out exactly which commit the bridge was built from.
// These are filled at build time with the -X linker flag.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

//go:embed example-config.yaml
var ExampleConfig string

type WABridge struct {
	bridge.Bridge
	Config       *config.Config
	DB           *database.Database
	Provisioning *ProvisioningAPI
	Formatter    *Formatter
	Metrics      *MetricsHandler
	WAContainer  *sqlstore.Container
	WAVersion    string

	PuppetActivity *PuppetActivity

	usersByMXID         map[id.UserID]*User
	usersByUsername     map[string]*User
	usersLock           sync.Mutex
	spaceRooms          map[id.RoomID]*User
	spaceRoomsLock      sync.Mutex
	managementRooms     map[id.RoomID]*User
	managementRoomsLock sync.Mutex
	portalsByMXID       map[id.RoomID]*Portal
	portalsByJID        map[database.PortalKey]*Portal
	portalsLock         sync.Mutex
	puppets             map[types.JID]*Puppet
	puppetsByCustomMXID map[id.UserID]*Puppet
	puppetsLock         sync.Mutex
}

func (br *WABridge) Init() {
	br.CommandProcessor = commands.NewProcessor(&br.Bridge)
	br.RegisterCommands()

	// TODO this is a weird place for this
	br.EventProcessor.On(event.EphemeralEventPresence, br.HandlePresence)

	Segment.log = br.Log.Sub("Segment")
	Segment.key = br.Config.SegmentKey
	if Segment.IsEnabled() {
		Segment.log.Infoln("Segment metrics are enabled")
	}

	br.DB = database.New(br.Bridge.DB)
	br.WAContainer = sqlstore.NewWithDB(br.DB.DB, br.DB.Dialect.String(), &waLogger{br.DB.Log.Sub("WhatsApp")})
	br.WAContainer.DatabaseErrorHandler = br.DB.HandleSignalStoreError

	ss := br.Config.Bridge.Provisioning.SharedSecret
	if len(ss) > 0 && ss != "disable" {
		br.Provisioning = &ProvisioningAPI{bridge: br}
	}

	br.Formatter = NewFormatter(br)
	br.Metrics = NewMetricsHandler(br.Config.Metrics.Listen, br.Log.Sub("Metrics"), br.DB, br.PuppetActivity)
	br.MatrixHandler.TrackEventDuration = br.Metrics.TrackMatrixEvent

	store.BaseClientPayload.UserAgent.OsVersion = proto.String(br.WAVersion)
	store.BaseClientPayload.UserAgent.OsBuildNumber = proto.String(br.WAVersion)
	store.DeviceProps.Os = proto.String(br.Config.WhatsApp.OSName)
	store.DeviceProps.RequireFullSync = proto.Bool(br.Config.Bridge.HistorySync.RequestFullSync)
	versionParts := strings.Split(br.WAVersion, ".")
	if len(versionParts) > 2 {
		primary, _ := strconv.Atoi(versionParts[0])
		secondary, _ := strconv.Atoi(versionParts[1])
		tertiary, _ := strconv.Atoi(versionParts[2])
		store.DeviceProps.Version.Primary = proto.Uint32(uint32(primary))
		store.DeviceProps.Version.Secondary = proto.Uint32(uint32(secondary))
		store.DeviceProps.Version.Tertiary = proto.Uint32(uint32(tertiary))
	}
	platformID, ok := waProto.DeviceProps_DevicePropsPlatformType_value[strings.ToUpper(br.Config.WhatsApp.BrowserName)]
	if ok {
		store.DeviceProps.PlatformType = waProto.DeviceProps_DevicePropsPlatformType(platformID).Enum()
	}
}

func (br *WABridge) Start() {
	err := br.WAContainer.Upgrade()
	if err != nil {
		br.Log.Fatalln("Failed to upgrade whatsmeow database: %v", err)
		os.Exit(15)
	}
	if br.Provisioning != nil {
		br.Log.Debugln("Initializing provisioning API")
		br.Provisioning.Init()
	}
	go br.CheckWhatsAppUpdate()
	go br.StartUsers()
	br.UpdateActivePuppetCount()
	if br.Config.Metrics.Enabled {
		go br.Metrics.Start()
	}

	if br.Config.Bridge.ResendBridgeInfo {
		go br.ResendBridgeInfo()
	}
	go br.Loop()
}

func (br *WABridge) CheckWhatsAppUpdate() {
	br.Log.Debugfln("Checking for WhatsApp web update")
	resp, err := whatsmeow.CheckUpdate(http.DefaultClient)
	if err != nil {
		br.Log.Warnfln("Failed to check for WhatsApp web update: %v", err)
		return
	}
	if store.GetWAVersion() == resp.ParsedVersion {
		br.Log.Debugfln("Bridge is using latest WhatsApp web protocol")
	} else if store.GetWAVersion().LessThan(resp.ParsedVersion) {
		if resp.IsBelowHard || resp.IsBroken {
			br.Log.Warnfln("Bridge is using outdated WhatsApp web protocol and probably doesn't work anymore (%s, latest is %s)", store.GetWAVersion(), resp.ParsedVersion)
		} else if resp.IsBelowSoft {
			br.Log.Infofln("Bridge is using outdated WhatsApp web protocol (%s, latest is %s)", store.GetWAVersion(), resp.ParsedVersion)
		} else {
			br.Log.Debugfln("Bridge is using outdated WhatsApp web protocol (%s, latest is %s)", store.GetWAVersion(), resp.ParsedVersion)
		}
	} else {
		br.Log.Debugfln("Bridge is using newer than latest WhatsApp web protocol")
	}
}

func (br *WABridge) Loop() {
	for {
		br.SleepAndDeleteUpcoming()
		time.Sleep(1 * time.Hour)
		br.WarnUsersAboutDisconnection()
	}
}

func (br *WABridge) WarnUsersAboutDisconnection() {
	br.usersLock.Lock()
	for _, user := range br.usersByUsername {
		if user.IsConnected() && !user.PhoneRecentlySeen(true) {
			go user.sendPhoneOfflineWarning()
		}
	}
	br.usersLock.Unlock()
}

func (br *WABridge) ResendBridgeInfo() {
	// FIXME
	//if *dontSaveConfig {
	//	br.Log.Warnln("Not setting resend_bridge_info to false in config due to --no-update flag")
	//} else {
	//	err := config.Mutate(*configPath, func(helper *configupgrade.Helper) {
	//		helper.Set(configupgrade.Bool, "false", "bridge", "resend_bridge_info")
	//	})
	//	if err != nil {
	//		br.Log.Errorln("Failed to save config after setting resend_bridge_info to false:", err)
	//	}
	//}
	//br.Log.Infoln("Re-sending bridge info state event to all portals")
	//for _, portal := range br.GetAllPortals() {
	//	portal.UpdateBridgeInfo()
	//}
	//br.Log.Infoln("Finished re-sending bridge info state events")
}

func (br *WABridge) StartUsers() {
	br.Log.Debugln("Starting users")
	foundAnySessions := false
	for _, user := range br.GetAllUsers() {
		if !user.JID.IsEmpty() {
			foundAnySessions = true
		}
		go user.Connect()
	}
	if !foundAnySessions {
		br.SendGlobalBridgeState(bridge.State{StateEvent: bridge.StateUnconfigured}.Fill(nil))
	}
	br.Log.Debugln("Starting custom puppets")
	for _, loopuppet := range br.GetAllPuppetsWithCustomMXID() {
		go func(puppet *Puppet) {
			puppet.log.Debugln("Starting custom puppet", puppet.CustomMXID)
			err := puppet.StartCustomMXID(true)
			if err != nil {
				puppet.log.Errorln("Failed to start custom puppet:", err)
			}
		}(loopuppet)
	}
}

func (br *WABridge) Stop() {
	br.Metrics.Stop()
	for _, user := range br.usersByUsername {
		if user.Client == nil {
			continue
		}
		br.Log.Debugln("Disconnecting", user.MXID)
		user.Client.Disconnect()
		close(user.historySyncs)
	}
}

func (br *WABridge) GetExampleConfig() string {
	return ExampleConfig
}

func (br *WABridge) GetConfigPtr() interface{} {
	br.Config = &config.Config{
		BaseConfig: &br.Bridge.Config,
	}
	br.Config.BaseConfig.Bridge = &br.Config.Bridge
	return br.Config
}

func main() {
	br := &WABridge{
		usersByMXID:         make(map[id.UserID]*User),
		usersByUsername:     make(map[string]*User),
		spaceRooms:          make(map[id.RoomID]*User),
		managementRooms:     make(map[id.RoomID]*User),
		portalsByMXID:       make(map[id.RoomID]*Portal),
		portalsByJID:        make(map[database.PortalKey]*Portal),
		puppets:             make(map[types.JID]*Puppet),
		puppetsByCustomMXID: make(map[id.UserID]*Puppet),
		PuppetActivity: &PuppetActivity{
			currentUserCount: 0,
			isBlocked:        false,
		},
	}
	br.Bridge = bridge.Bridge{
		Name:         "mautrix-whatsapp",
		URL:          "https://github.com/vector-im/mautrix-whatsapp",
		Description:  "A Matrix-WhatsApp puppeting bridge (Element fork).",
		Version:      "0.5.0-mod-1",
		ProtocolName: "WhatsApp",

		CryptoPickleKey: "maunium.net/go/mautrix-whatsapp",

		ConfigUpgrader: &configupgrade.StructUpgrader{
			SimpleUpgrader: configupgrade.SimpleUpgrader(config.DoUpgrade),
			Blocks:         config.SpacedBlocks,
			Base:           ExampleConfig,
		},

		Child: br,
	}
	br.InitVersion(Tag, Commit, BuildTime)
	br.WAVersion = strings.FieldsFunc(br.Version, func(r rune) bool { return r == '-' || r == '+' })[0]

	br.Main()
}

func (mh *WABridge) UpdateActivePuppetCount() {
	mh.Log.Debugfln("Updating active puppet count")

	var minActivityTime = int64(ONE_DAY_S * mh.Config.Limits.MinPuppetActiveDays)
	var maxActivityTime = int64(ONE_DAY_S * mh.Config.Limits.PuppetInactivityDays)
	var activePuppetCount uint
	var firstActivityTs, lastActivityTs int64

	rows, active_err := mh.DB.Query("SELECT first_activity_ts, last_activity_ts FROM puppet WHERE first_activity_ts is not NULL")
	if active_err != nil {
		mh.Log.Warnln("Failed to scan number of active puppets:", active_err)
	} else {
		defer rows.Close()
		for rows.Next() {
			rows.Scan(&firstActivityTs, &lastActivityTs)
			var secondsOfActivity = lastActivityTs - firstActivityTs
			var isInactive = time.Now().Unix()-lastActivityTs > maxActivityTime
			if !isInactive && secondsOfActivity > minActivityTime && secondsOfActivity < maxActivityTime {
				activePuppetCount++
			}
		}
		if mh.Config.Limits.BlockOnLimitReached {
			mh.PuppetActivity.isBlocked = mh.Config.Limits.MaxPuppetLimit < activePuppetCount
		}
		mh.Log.Debugfln("Current active puppet count is %d (max %d)", activePuppetCount, mh.Config.Limits.MaxPuppetLimit)
		mh.PuppetActivity.currentUserCount = activePuppetCount
	}
}
