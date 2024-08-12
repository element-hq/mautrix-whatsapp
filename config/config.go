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

package config

import (
	"github.com/element-hq/mautrix-go/bridge/bridgeconfig"
)

type Config struct {
	*bridgeconfig.BaseConfig `yaml:",inline"`

	Analytics struct {
		Host   string `yaml:"host"`
		Token  string `yaml:"token"`
		UserID string `yaml:"user_id"`
	}

	Limits struct {
		MaxPuppetLimit       uint `yaml:"max_puppet_limit"`
		MinPuppetActiveDays  uint `yaml:"min_puppet_activity_days"`
		PuppetInactivityDays uint `yaml:"puppet_inactivity_days"`
		BlockOnLimitReached  bool `yaml:"block_on_limit_reached"`
	} `yaml:"limits"`

	Metrics struct {
		Enabled bool   `yaml:"enabled"`
		Listen  string `yaml:"listen"`
	} `yaml:"metrics"`

	WhatsApp struct {
		OSName      string `yaml:"os_name"`
		BrowserName string `yaml:"browser_name"`

		Proxy          string `yaml:"proxy"`
		GetProxyURL    string `yaml:"get_proxy_url"`
		ProxyOnlyLogin bool   `yaml:"proxy_only_login"`
	} `yaml:"whatsapp"`

	Bridge BridgeConfig `yaml:"bridge"`
}
