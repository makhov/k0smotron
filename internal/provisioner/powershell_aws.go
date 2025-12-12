/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provisioner

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
)

// PowerShellAWSProvisioner implements the Provisioner interface for cloud-init.
type PowerShellAWSProvisioner struct{}

// ToProvisionData converts the input data to aws windows user data.
func (c *PowerShellAWSProvisioner) ToProvisionData(input *InputProvisionData) ([]byte, error) {
	var b bytes.Buffer

	// Write the "header" first
	_, err := b.WriteString("<powershell>\n")
	if err != nil {
		return nil, err
	}
	_, _ = b.WriteString("$ErrorActionPreference = \"Stop\"\n")
	_, _ = b.WriteString("Start-Transcript -Path C:\\bootstrap.log -Append\n")

	// ---- write_files ----
	for _, f := range input.Files {
		renderWriteFile(&b, f)
	}

	_, _ = b.WriteString("if (Test-Path C:\\bootstrap.done) {\n    Write-Host \"Bootstrap already completed, skipping.\"\n    exit 0\n}\n")

	// ---- runcmd ----
	if len(input.Commands) > 0 {
		b.WriteString("\n# --- runcmd ---\n")
		for _, cmd := range input.Commands {
			b.WriteString(cmd)
			b.WriteString("\n")
		}
	}

	if input.CustomUserData != "" {
		_, err = b.WriteString(input.CustomUserData)
		if err != nil {
			return nil, err
		}
	}

	_, err = b.WriteString("New-Item C:\\bootstrap.done -ItemType File\n\n")
	// Write the "footer"
	_, _ = b.WriteString("Stop-Transcript\n")
	_, err = b.WriteString("</powershell>\n")
	if err != nil {
		return nil, err
	}
	_, _ = b.WriteString("<persist>true</persist>\n")

	content := strings.ReplaceAll(b.String(), "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	//	content := `version: 1.1
	//tasks:
	//- task: executeScript
	//  inputs:
	//  - frequency: always
	//    type: powershell
	//    runAs: localSystem
	//    content: |-
	//      New-Item -Path 'C:\PowerShellTest.txt' -ItemType File
	//      try {
	//        Start-Transcript -Path C:\bootstrap.log -Append
	//        Write-Host "Transcript started"
	//      } catch {
	//        New-Item -ItemType File -Path C:\TRANSCRIPT_FAILED.txt -Force | Out-Null
	//      }`
	return []byte(content), nil
}

// GetFormat returns the format 'cloud-config' of the provisioner.
func (c *PowerShellAWSProvisioner) GetFormat() string {
	return powershellAWSProvisioningFormat
}

func renderWriteFile(buf *bytes.Buffer, f File) {
	dir := filepath.Dir(strings.Replace(f.Path, `\`, `/`, -1))

	buf.WriteString("\n# --- write_file ---\n")

	// Ensure directory exists
	buf.WriteString(fmt.Sprintf(
		"New-Item -ItemType Directory -Force -Path \"%s\" | Out-Null\n",
		escapePS(dir),
	))

	content := normalizeNewlines(f.Content)

	// Here-string write
	buf.WriteString("$file = @'\n")
	buf.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		buf.WriteString("\n")
	}
	buf.WriteString("'@\n")
	buf.WriteString(fmt.Sprintf(`[System.IO.File]::WriteAllText(
  "%s",
  $file.Trim(),
  [System.Text.Encoding]::ASCII
)`+"\n", escapePS(f.Path)))
}

func escapePS(s string) string {
	// PowerShell double-quoted string escaping
	return strings.ReplaceAll(s, `"`, `""`)
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}
