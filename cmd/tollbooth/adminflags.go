// Copyright 2026 Blake Golliher
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
)

// adminFlags are the flags every verb that talks to the running proxy shares (docs/SPEC.md,
// Operator surface): the admin endpoint and token come from --admin and --token, then
// TOLLBOOTH_ADMIN and TOLLBOOTH_TOKEN, then the config file; the output format is one of table,
// json (the admin response unwrapped), or csv.
type adminFlags struct {
	admin  string
	token  string
	config string
	format string
}

func (a *adminFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&a.admin, "admin", "", "admin API address (host:port or http://host:port); default from TOLLBOOTH_ADMIN, then the config file")
	fs.StringVar(&a.token, "token", "", "admin bearer token; default from TOLLBOOTH_TOKEN, then the config file")
	fs.StringVar(&a.config, "config", "tollbooth.yaml", "config file, read only for admin.address and the token when the flags and variables are unset")
	fs.StringVar(&a.format, "format", "table", "output format: table, json, or csv")
}

// client resolves the endpoint and token and returns the admin client. The config file is read
// only when needed and only if it exists; a missing file is not an error when the flags or the
// environment supplied everything.
func (a *adminFlags) client() (*admin.Client, error) {
	switch a.format {
	case "table", "json", "csv":
	default:
		return nil, fmt.Errorf("--format must be table, json, or csv, got %q", a.format)
	}
	addr, token := a.admin, a.token
	if addr == "" {
		addr = os.Getenv("TOLLBOOTH_ADMIN")
	}
	if token == "" {
		token = os.Getenv("TOLLBOOTH_TOKEN")
	}
	if addr == "" || token == "" {
		if cfg, err := config.Load(a.config); err == nil {
			if addr == "" {
				addr = cfg.Admin.Address
			}
			if token == "" {
				token = cfg.AdminToken()
			}
		} else if addr == "" && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("--admin not set and %s: %w", a.config, err)
		}
	}
	if addr == "" {
		addr = config.DefaultAdminAddress
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &admin.Client{Base: addr, Token: token}, nil
}
