package cli

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWindowsDesktopCaptureUsesInteractiveScheduledTasksAndCleansUp(t *testing.T) {
	transport := &fakeWindowsDesktopTransport{
		outputs: [][]byte{
			nil,
			validJPEGBytes(t),
			nil,
			zipFrameArchive(t),
		},
	}
	ffmpeg := writeFakeFFmpeg(t, t.TempDir())

	screenshot, err := captureWindowsDesktopScreenshot(transport, "C:/maya-stall/artifacts/proof")
	if err != nil {
		t.Fatalf("captureWindowsDesktopScreenshot returned error: %v", err)
	}
	if !looksLikeImageBytes("image/png", screenshot) {
		t.Fatalf("screenshot bytes do not contain a PNG transcoded from the Windows-host JPEG: %v", screenshot)
	}

	recording, err := captureWindowsDesktopRecording(transport, "C:/maya-stall/artifacts/proof", 2*time.Second, 2, ffmpeg)
	if err != nil {
		t.Fatalf("captureWindowsDesktopRecording returned error: %v", err)
	}
	if !looksLikeMP4Bytes(recording) {
		t.Fatalf("recording bytes do not look like MP4: %v", recording)
	}

	combined := strings.Join(append(transport.scripts, transport.writes...), "\n")
	for _, want := range []string{
		"System.Windows.Forms",
		"System.Drawing",
		"ImageFormat]::Jpeg",
		"schtasks.exe",
		"/IT",
		"LIMITED",
		"Compress-Archive",
		"Remove-Item -Recurse -Force",
		"interactive desktop session is logged in",
		"Windows PowerShell desktop assemblies",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("desktop capture commands missing %q:\n%s", want, combined)
		}
	}
	if strings.Contains(combined, "viewport.capture") {
		t.Fatalf("desktop capture must not use viewport.capture:\n%s", combined)
	}
	if strings.Contains(combined, "HIGHEST") {
		t.Fatalf("desktop capture must not require an elevated scheduled task:\n%s", combined)
	}
}

func TestWindowsDesktopScreenshotTranscodesHostJPEGToPNG(t *testing.T) {
	transport := &fakeWindowsDesktopTransport{outputs: [][]byte{nil, validJPEGBytes(t)}}
	screenshot, err := captureWindowsDesktopScreenshot(transport, "C:/maya-stall/artifacts/proof")
	if err != nil {
		t.Fatalf("captureWindowsDesktopScreenshot returned error: %v", err)
	}
	if !looksLikeImageBytes("image/png", screenshot) {
		t.Fatalf("screenshot bytes do not contain a PNG transcoded from the Windows-host JPEG: %v", screenshot)
	}
	if script := strings.Join(append(transport.scripts, transport.writes...), "\n"); !strings.Contains(script, `desktop-screenshot.jpg`) || !strings.Contains(script, `ImageFormat]::Jpeg`) {
		t.Fatalf("desktop screenshot host script does not capture JPEG bytes:\n%s", script)
	}
}

func TestWindowsDesktopScreenshotStagesLongControllerScript(t *testing.T) {
	transport := &fakeWindowsDesktopTransport{outputs: [][]byte{nil, validJPEGBytes(t)}}
	if _, err := captureWindowsDesktopScreenshot(transport, "C:/maya-stall/artifacts/proof"); err != nil {
		t.Fatalf("captureWindowsDesktopScreenshot returned error: %v", err)
	}
	if len(transport.writes) != 1 || !strings.Contains(transport.writes[0], "desktop-screenshot-controller.ps1\n") || !strings.Contains(transport.writes[0], "MayaStallVisualEvidenceScreenshot-") {
		t.Fatalf("desktop screenshot controller was not staged as a PowerShell file: %+v", transport.writes)
	}
	if len(transport.scripts) != 2 || !strings.Contains(transport.scripts[0], "New-Item -ItemType Directory") || transport.scripts[1] != `& 'C:/maya-stall/artifacts/proof/desktop-screenshot-controller.ps1'` {
		t.Fatalf("desktop screenshot staged invocation = %+v", transport.scripts)
	}
}

func TestWindowsDesktopScreenshotWaitsForCompletedImage(t *testing.T) {
	script := windowsDesktopScreenshotPowerShell("C:/maya-stall/artifacts/proof")
	if !strings.Contains(script, `$done = $out + ".done"`) || !strings.Contains(script, `Set-Content -LiteralPath ("__MAYA_STALL_SCREENSHOT_OUT__" + ".done") -Value "ok"`) {
		t.Fatalf("desktop screenshot task must publish an explicit completion marker after saving the image:\n%s", script)
	}
	if !strings.Contains(script, `(Test-Path -LiteralPath $done) -and (Test-Path -LiteralPath $out)`) {
		t.Fatalf("desktop screenshot controller must wait for the completion marker before reading the image:\n%s", script)
	}
}

func TestSSHWindowsDesktopTransportStreamsLongPowerShellOverStdin(t *testing.T) {
	script := windowsDesktopScreenshotPowerShell("C:/maya-stall/runs/run-123/visual-evidence/screenshot")
	encoded := strings.Join(encodedPowerShellCommand(script), " ")
	if len(encoded) < 8000 {
		t.Fatalf("desktop screenshot encoded command length = %d, want proof it leaves unsafe headroom under the Windows default-shell command-line limit", len(encoded))
	}
	command := strings.Join(windowsPowerShellStdinCommand(), " ")
	if !strings.Contains(command, "-Command -") || strings.Contains(command, "-EncodedCommand") {
		t.Fatalf("SSH desktop PowerShell command must read the script from stdin: %s", command)
	}
}

func validJPEGBytes(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	pixels := image.NewRGBA(image.Rect(0, 0, 2, 2))
	pixels.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(&output, pixels, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode JPEG fixture: %v", err)
	}
	return output.Bytes()
}

func TestWindowsDesktopCaptureUsesFullVirtualDesktopBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{name: "screenshot", script: windowsDesktopScreenshotPowerShell("C:/maya-stall/artifacts/proof")},
		{name: "recording", script: windowsDesktopRecordingPowerShell("C:/maya-stall/artifacts/proof", 3, 500)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.script, "PrimaryScreen") {
				t.Fatalf("%s capture must not use primary-screen-only bounds:\n%s", tc.name, tc.script)
			}
			if !strings.Contains(tc.script, "[System.Windows.Forms.SystemInformation]::VirtualScreen") {
				t.Fatalf("%s capture must use full virtual desktop bounds:\n%s", tc.name, tc.script)
			}
		})
	}
}

func TestWindowsDesktopRecordingFailsClearlyWithoutLocalFFmpeg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	transport := &fakeWindowsDesktopTransport{}
	_, err := captureWindowsDesktopRecording(transport, "C:/maya-stall/artifacts/proof", time.Second, 1, "")
	if err == nil || !strings.Contains(err.Error(), "local ffmpeg is required") {
		t.Fatalf("recording error = %v, want local ffmpeg requirement", err)
	}
}

func TestWindowsDesktopCapturePreservesPowerShellPrerequisiteErrors(t *testing.T) {
	transport := &fakeWindowsDesktopTransport{err: errors.New("schtasks.exe is required for interactive desktop capture")}
	_, err := captureWindowsDesktopScreenshot(transport, "C:/maya-stall/artifacts/proof")
	if err == nil || !strings.Contains(err.Error(), "schtasks.exe is required") {
		t.Fatalf("screenshot error = %v, want schtasks prerequisite detail", err)
	}
}

func TestWindowsDesktopClickUsesInteractiveScheduledTaskAndSendInput(t *testing.T) {
	transport := &fakeWindowsDesktopTransport{}

	if err := clickWindowsDesktop(transport, "C:/maya-stall/artifacts/control", 12, 34); err != nil {
		t.Fatalf("clickWindowsDesktop returned error: %v", err)
	}

	combined := strings.Join(transport.scripts, "\n")
	for _, want := range []string{
		"schtasks.exe",
		"/IT",
		"LIMITED",
		"user32.dll",
		"MoveAndClick(12, 34)",
		"GetSystemMetrics(76)",
		"dwFlags = 0xC001",
		"SendInput",
		"inserted != (uint)inputs.Length",
		"$deadline = (Get-Date).AddSeconds(30)",
		"while ((Get-Date) -lt $deadline)",
		"Remove-Item -Recurse -Force",
		"interactive desktop session is logged in",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("desktop click command missing %q:\n%s", want, combined)
		}
	}
	if strings.Contains(combined, "HIGHEST") {
		t.Fatalf("desktop click must not require an elevated scheduled task:\n%s", combined)
	}
	if strings.Contains(combined, "mouse_event") {
		t.Fatalf("desktop click must use one serial SendInput batch instead of superseded mouse_event calls:\n%s", combined)
	}
	if strings.Contains(combined, "SetCursorPos") {
		t.Fatalf("desktop click must atomically move and click in one SendInput batch:\n%s", combined)
	}
}

func TestWindowsDesktopClickSendInputSourceCompilesOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell Add-Type proof requires Windows")
	}
	script := windowsDesktopClickPowerShell("C:/maya-stall/artifacts/control", 12, 34)
	const prefix = "$source = @\"\n"
	const suffix = "\n\"@\nAdd-Type -TypeDefinition $source"
	start := strings.Index(script, prefix)
	if start < 0 {
		t.Fatalf("desktop click command missing C# source prefix:\n%s", script)
	}
	start += len(prefix)
	end := strings.Index(script[start:], suffix)
	if end < 0 {
		t.Fatalf("desktop click command missing C# source suffix:\n%s", script)
	}
	source := script[start : start+end]
	command := "$ErrorActionPreference = 'Stop'\n$source = @'\n" + source + "\n'@\nAdd-Type -TypeDefinition $source"
	output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
	if err != nil {
		t.Fatalf("compile generated desktop click SendInput source: %v: %s", err, output)
	}
}

func TestWindowsDesktopClickRejectsNegativeCoordinates(t *testing.T) {
	transport := &fakeWindowsDesktopTransport{}
	err := clickWindowsDesktop(transport, "C:/maya-stall/artifacts/control", -1, 34)
	if err == nil || !strings.Contains(err.Error(), "desktop click coordinates must be non-negative") {
		t.Fatalf("click error = %v, want coordinate validation", err)
	}
	if len(transport.scripts) != 0 {
		t.Fatalf("click should not run PowerShell for invalid coordinates: %+v", transport.scripts)
	}
}

func TestWriteRemotePowerShellScriptRedactsConfiguredEndpointFromFailure(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-private-recording-endpoint")
	script := "#!/bin/sh\nprintf 'ssh: connect to host maya-private.example port 22: Operation timed out\\n' >&2\nexit 255\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-private.example", Binary: sshPath}}

	err := writeRemotePowerShellScript(host, "C:/maya-stall/record.ps1", "Write-Output ok", sshCommandTimeout)
	if err == nil {
		t.Fatal("failed recording-script upload returned no error")
	}
	if strings.Contains(err.Error(), host.SSH.Host) || !strings.Contains(err.Error(), "SSH operation reported an error") {
		t.Fatalf("recording-script SSH error exposed configured endpoint: %v", err)
	}
}

type fakeWindowsDesktopTransport struct {
	scripts []string
	writes  []string
	outputs [][]byte
	err     error
}

func (transport *fakeWindowsDesktopTransport) RunPowerShell(script string, timeout time.Duration) ([]byte, error) {
	transport.scripts = append(transport.scripts, script)
	if transport.err != nil {
		return nil, transport.err
	}
	if len(transport.outputs) == 0 {
		return nil, nil
	}
	output := transport.outputs[0]
	transport.outputs = transport.outputs[1:]
	return output, nil
}

func (transport *fakeWindowsDesktopTransport) WritePowerShellScript(remotePath string, content string, timeout time.Duration) error {
	transport.writes = append(transport.writes, remotePath+"\n"+content)
	return transport.err
}

func zipFrameArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range []string{"frame-000000.jpg", "frame-000001.jpg"} {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip frame: %v", err)
		}
		if _, err := file.Write(jpegHeaderBytes()); err != nil {
			t.Fatalf("write zip frame: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buffer.Bytes()
}

func writeFakeFFmpeg(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ffmpeg")
	content := "#!/bin/sh\nfor out do :; done\nprintf '\\000\\000\\000\\030ftypmp42' > \"$out\"\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}
