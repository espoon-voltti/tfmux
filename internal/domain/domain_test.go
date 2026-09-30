// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package domain

import "testing"

func TestInitUsesWorkspace(t *testing.T) {
	cases := []struct {
		name string
		c    *CommandTemplates
		want bool
	}{
		{"nil", nil, false},
		{"plain", &CommandTemplates{Init: `"$TFMUX_TF_BIN" init`}, false},
		{"bare var", &CommandTemplates{Init: "terraform init -backend-config=$TFMUX_WORKSPACE.hcl"}, true},
		{"braced expansion", &CommandTemplates{Init: "terraform init -backend-config=${TFMUX_WORKSPACE##*-}.hcl"}, true},
		{"only plan uses it", &CommandTemplates{Init: "terraform init", Plan: "terraform plan -var-file=$TFMUX_WORKSPACE.tfvars"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.c.InitUsesWorkspace(); got != c.want {
				t.Errorf("InitUsesWorkspace() = %v, want %v", got, c.want)
			}
		})
	}
}
