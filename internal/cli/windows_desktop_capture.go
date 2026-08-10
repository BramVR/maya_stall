package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type windowsDesktopTransport interface {
	RunPowerShell(script string, timeout time.Duration) ([]byte, error)
	WritePowerShellScript(remotePath string, content string, timeout time.Duration) error
}

type sshWindowsDesktopTransport mayaHostConfig

type localWindowsDesktopTransport struct {
	workRoot string
}

var lookPath = exec.LookPath

func (transport sshWindowsDesktopTransport) RunPowerShell(script string, timeout time.Duration) ([]byte, error) {
	return runSSHCommandOutputWithInput(mayaHostConfig(transport), windowsPowerShellStdinCommand(), script, timeout)
}

func windowsPowerShellStdinCommand() []string {
	return []string{"powershell", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "-"}
}

func (transport sshWindowsDesktopTransport) WritePowerShellScript(remotePath string, content string, timeout time.Duration) error {
	return writeRemotePowerShellScript(mayaHostConfig(transport), remotePath, content, timeout)
}

func (transport localWindowsDesktopTransport) RunPowerShell(script string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = sessiondCommandTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	encoded := base64.StdEncoding.EncodeToString(utf16LEBytes(script))
	command := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encoded)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	raw, err := command.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("run local Windows PowerShell timed out after %s", timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("run local Windows PowerShell: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return raw, nil
}

func utf16LEBytes(value string) []byte {
	runes := []rune(value)
	encoded := make([]byte, 0, len(runes)*2)
	for _, character := range runes {
		if character <= 0xffff {
			encoded = append(encoded, byte(character), byte(character>>8))
			continue
		}
		character -= 0x10000
		high := uint16(0xd800 + (character >> 10))
		low := uint16(0xdc00 + (character & 0x3ff))
		encoded = append(encoded, byte(high), byte(high>>8), byte(low), byte(low>>8))
	}
	return encoded
}

func (transport localWindowsDesktopTransport) WritePowerShellScript(path string, content string, _ time.Duration) error {
	localPath := filepath.FromSlash(path)
	if err := requireLocalWindowsPathWithin(filepath.FromSlash(remoteJoin(transport.workRoot, "runs")), localPath); err != nil {
		return err
	}
	if err := rejectLocalWindowsReparseAncestors(localPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(localPath, []byte(content), 0o644)
}

func captureWindowsDesktopScreenshot(transport windowsDesktopTransport, remoteRoot string) ([]byte, error) {
	data, err := transport.RunPowerShell(windowsDesktopScreenshotPowerShell(remoteRoot), sessiondCommandTimeout)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("windows desktop screenshot capture returned no image bytes")
	}
	image, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode Windows desktop screenshot JPEG: %w", err)
	}
	var output bytes.Buffer
	if err := png.Encode(&output, image); err != nil {
		return nil, fmt.Errorf("encode Windows desktop screenshot PNG: %w", err)
	}
	return output.Bytes(), nil
}

func captureLocalWindowsDesktopScreenshot(transport windowsDesktopTransport) ([]byte, error) {
	data, err := transport.RunPowerShell(localWindowsDesktopScreenshotPowerShell(), sessiondCommandTimeout)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("local Windows desktop screenshot capture returned no image bytes")
	}
	return data, nil
}

func localWindowsDesktopScreenshotPowerShell() string {
	return `$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$bounds = [System.Windows.Forms.SystemInformation]::VirtualScreen
if ($bounds.Width -le 0 -or $bounds.Height -le 0) { throw "interactive desktop session is unavailable for screenshot capture" }
$bitmap = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
$graphics = [System.Drawing.Graphics]::FromImage($bitmap)
$stream = New-Object IO.MemoryStream
try {
  $graphics.CopyFromScreen($bounds.Location, [System.Drawing.Point]::Empty, $bounds.Size)
  $bitmap.Save($stream, [System.Drawing.Imaging.ImageFormat]::Png)
  $data = $stream.ToArray()
  [Console]::OpenStandardOutput().Write($data, 0, $data.Length)
} finally {
  $stream.Dispose()
  $graphics.Dispose()
  $bitmap.Dispose()
}`
}

func clickWindowsDesktop(transport windowsDesktopTransport, remoteRoot string, x int, y int) error {
	if x < 0 || y < 0 {
		return fmt.Errorf("desktop click coordinates must be non-negative")
	}
	_, err := transport.RunPowerShell(windowsDesktopClickPowerShell(remoteRoot, x, y), sessiondCommandTimeout)
	return err
}

func captureWindowsDesktopRecording(transport windowsDesktopTransport, remoteRoot string, duration time.Duration, fps int, ffmpegPath string) ([]byte, error) {
	if strings.TrimSpace(ffmpegPath) == "" {
		found, err := lookPath("ffmpeg")
		if err != nil {
			return nil, fmt.Errorf("local ffmpeg is required for Windows desktop recording capture: %w", err)
		}
		ffmpegPath = found
	}
	frames, intervalMS := windowsDesktopFrameTiming(duration, fps)
	if _, err := transport.RunPowerShell(fmt.Sprintf("New-Item -ItemType Directory -Force -Path %s | Out-Null", powerShellSingleQuoted(remoteRoot)), sessiondCommandTimeout); err != nil {
		return nil, err
	}
	scriptPath := remoteJoin(remoteRoot, "desktop-recording.ps1")
	if err := transport.WritePowerShellScript(scriptPath, windowsDesktopRecordingPowerShell(remoteRoot, frames, intervalMS), sessiondCommandTimeout); err != nil {
		return nil, err
	}
	zipBytes, err := transport.RunPowerShell(fmt.Sprintf("Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force; & %s", powerShellSingleQuoted(scriptPath)), duration+sessiondCommandTimeout)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "maya-stall-windows-video-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()
	framesDir := filepath.Join(tempDir, "frames")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		return nil, err
	}
	if err := extractWindowsDesktopFrameArchive(zipBytes, framesDir); err != nil {
		return nil, err
	}
	outputPath := filepath.Join(tempDir, "desktop-recording.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), duration+sessiondCommandTimeout)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-framerate", strconv.Itoa(fps),
		"-start_number", "0",
		"-i", filepath.Join(framesDir, "frame-%06d.jpg"),
		"-pix_fmt", "yuv420p",
		"-an",
		"-movflags", "+faststart",
		outputPath,
	}
	if out, err := exec.CommandContext(ctx, ffmpegPath, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("encode Windows desktop frames with ffmpeg: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(outputPath)
}

func writeRemotePowerShellScript(host mayaHostConfig, remotePath string, content string, timeout time.Duration) error {
	binary := host.SSH.Binary
	if binary == "" {
		binary = "ssh"
	}
	if timeout <= 0 {
		timeout = sshCommandTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	script := fmt.Sprintf("$content = [Console]::In.ReadToEnd(); Set-Content -Encoding UTF8 -LiteralPath %s -Value $content", powerShellSingleQuoted(remotePath))
	command := exec.CommandContext(ctx, binary, append(sshArgs(host), encodedPowerShellCommand(script)...)...)
	command.Stdin = strings.NewReader(content)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("write remote PowerShell script timed out after %s", timeout)
		}
		detail := sshFailureDetail(err, stderr.String())
		err = sanitizedSSHExecutionErrorFor(err, stderr.String())
		if detail != "" {
			return fmt.Errorf("write remote PowerShell script failed: %w: %s", err, detail)
		}
		return fmt.Errorf("write remote PowerShell script failed: %w", err)
	}
	return nil
}

func windowsDesktopFrameTiming(duration time.Duration, fps int) (int, int) {
	if fps < 1 {
		fps = 1
	}
	frames := int(duration.Seconds()*float64(fps) + 0.999)
	if frames < 1 {
		frames = 1
	}
	intervalMS := 1000 / fps
	if intervalMS < 1 {
		intervalMS = 1
	}
	return frames, intervalMS
}

func windowsDesktopScreenshotPowerShell(remoteRoot string) string {
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$root = %s
New-Item -ItemType Directory -Force -Path $root | Out-Null
if (-not (Get-Command schtasks.exe -ErrorAction SilentlyContinue)) { throw "schtasks.exe is required for interactive desktop capture" }
$taskName = "MayaStallVisualEvidenceScreenshot-" + [Guid]::NewGuid().ToString("N")
$out = Join-Path $root "desktop-screenshot.jpg"
$done = $out + ".done"
$script = Join-Path $root ($taskName + ".ps1")
$template = @'
$ErrorActionPreference = "Stop"
try {
  Add-Type -AssemblyName System.Windows.Forms
  Add-Type -AssemblyName System.Drawing
} catch {
  throw "Windows PowerShell desktop assemblies System.Windows.Forms and System.Drawing are required for desktop screenshot capture: $($_.Exception.Message)"
}
$bounds = [System.Windows.Forms.SystemInformation]::VirtualScreen
if ($bounds.Width -le 0 -or $bounds.Height -le 0) { throw "interactive desktop session is unavailable for screenshot capture" }
$bitmap = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
$graphics = [System.Drawing.Graphics]::FromImage($bitmap)
$graphics.CopyFromScreen($bounds.Location, [System.Drawing.Point]::Empty, $bounds.Size)
$bitmap.Save("__MAYA_STALL_SCREENSHOT_OUT__", [System.Drawing.Imaging.ImageFormat]::Jpeg)
$graphics.Dispose()
$bitmap.Dispose()
Set-Content -LiteralPath ("__MAYA_STALL_SCREENSHOT_OUT__" + ".done") -Value "ok"
'@
try {
  $template.Replace("__MAYA_STALL_SCREENSHOT_OUT__", $out.Replace("\", "\\")) | Set-Content -Encoding ASCII -LiteralPath $script
  cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
  $startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
  $taskRun = 'powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $script + '"'
  $createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", $taskRun, "/RL", "LIMITED", "/IT", "/F")
  & schtasks.exe @createArgs | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "failed to create interactive desktop screenshot task with schtasks.exe /IT; ensure an interactive desktop session is logged in" }
  schtasks.exe /Run /TN $taskName | Out-Null
  for ($i = 0; $i -lt 40; $i++) {
    if ((Test-Path -LiteralPath $done) -and (Test-Path -LiteralPath $out) -and (Get-Item -LiteralPath $out).Length -gt 0) {
      try {
        $stream = [IO.File]::Open($out, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
        try {
          $buffer = New-Object byte[] 1048576
          while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
            [Console]::OpenStandardOutput().Write($buffer, 0, $read)
          }
        } finally {
          $stream.Dispose()
        }
        exit 0
      } catch {
        Start-Sleep -Milliseconds 500
      }
    }
    Start-Sleep -Milliseconds 500
  }
  throw "scheduled interactive desktop screenshot did not produce output; ensure an interactive desktop session is logged in"
} finally {
  schtasks.exe /Delete /TN $taskName /F | Out-Null
  Remove-Item -Recurse -Force -LiteralPath $root -ErrorAction SilentlyContinue
}`, powerShellSingleQuoted(remoteRoot))
}

func windowsDesktopClickPowerShell(remoteRoot string, x int, y int) string {
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$root = %s
New-Item -ItemType Directory -Force -Path $root | Out-Null
if (-not (Get-Command schtasks.exe -ErrorAction SilentlyContinue)) { throw "schtasks.exe is required for interactive desktop control" }
$taskName = "MayaStallDesktopClick-" + [Guid]::NewGuid().ToString("N")
$done = Join-Path $root "desktop-click.done"
$script = Join-Path $root ($taskName + ".ps1")
$template = @'
$ErrorActionPreference = "Stop"
$source = @"
using System;
using System.Runtime.InteropServices;
public static class MouseInput {
  [DllImport("user32.dll", SetLastError = true)]
  public static extern bool SetCursorPos(int X, int Y);
  [DllImport("user32.dll", SetLastError = true)]
  public static extern void mouse_event(int dwFlags, int dx, int dy, int dwData, UIntPtr dwExtraInfo);
}
"@
Add-Type -TypeDefinition $source
if (-not [MouseInput]::SetCursorPos(%d, %d)) { throw "SetCursorPos failed; interactive desktop session is unavailable for desktop control" }
Start-Sleep -Milliseconds 50
[MouseInput]::mouse_event(0x0002, 0, 0, 0, [UIntPtr]::Zero)
Start-Sleep -Milliseconds 50
[MouseInput]::mouse_event(0x0004, 0, 0, 0, [UIntPtr]::Zero)
Set-Content -LiteralPath "__MAYA_STALL_CLICK_DONE__" -Value "ok"
'@
try {
  $template.Replace("__MAYA_STALL_CLICK_DONE__", $done.Replace("\", "\\")) | Set-Content -Encoding ASCII -LiteralPath $script
  cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
  $startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
  $taskRun = 'powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $script + '"'
  $createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", $taskRun, "/RL", "LIMITED", "/IT", "/F")
  & schtasks.exe @createArgs | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "failed to create interactive desktop click task with schtasks.exe /IT; ensure an interactive desktop session is logged in" }
  schtasks.exe /Run /TN $taskName | Out-Null
  $deadline = (Get-Date).AddSeconds(30)
  while ((Get-Date) -lt $deadline) {
    if (Test-Path -LiteralPath $done) { exit 0 }
    Start-Sleep -Milliseconds 250
  }
  throw "scheduled interactive desktop click did not complete; ensure an interactive desktop session is logged in"
} finally {
  schtasks.exe /Delete /TN $taskName /F | Out-Null
  Remove-Item -Recurse -Force -LiteralPath $root -ErrorAction SilentlyContinue
}`, powerShellSingleQuoted(remoteRoot), x, y)
}

func windowsDesktopRecordingPowerShell(remoteRoot string, frames int, intervalMS int) string {
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$root = %s
New-Item -ItemType Directory -Force -Path $root | Out-Null
if (-not (Get-Command schtasks.exe -ErrorAction SilentlyContinue)) { throw "schtasks.exe is required for interactive desktop capture" }
$taskName = "MayaStallVisualEvidenceRecording-" + [Guid]::NewGuid().ToString("N")
$outDir = Join-Path $root "frames"
$zip = Join-Path $root "desktop-recording-frames.zip"
$done = $zip + ".done"
$script = Join-Path $root ($taskName + ".ps1")
$template = @'
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$OutDir = "__MAYA_STALL_FRAME_DIR__"
$Zip = "__MAYA_STALL_FRAME_ZIP__"
$Frames = __MAYA_STALL_FRAME_COUNT__
$IntervalMS = __MAYA_STALL_INTERVAL_MS__
try {
  Add-Type -AssemblyName System.Windows.Forms
  Add-Type -AssemblyName System.Drawing
} catch {
  throw "Windows PowerShell desktop assemblies System.Windows.Forms and System.Drawing are required for desktop recording capture: $($_.Exception.Message)"
}
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$bounds = [System.Windows.Forms.SystemInformation]::VirtualScreen
if ($bounds.Width -le 0 -or $bounds.Height -le 0) { throw "interactive desktop session is unavailable for recording capture" }
$start = [DateTime]::UtcNow
for ($i = 0; $i -lt $Frames; $i++) {
  $bitmap = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
  $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
  $graphics.CopyFromScreen($bounds.Location, [System.Drawing.Point]::Empty, $bounds.Size)
  $path = Join-Path $OutDir ("frame-{0:D6}.jpg" -f $i)
  $bitmap.Save($path, [System.Drawing.Imaging.ImageFormat]::Jpeg)
  $graphics.Dispose()
  $bitmap.Dispose()
  $target = $start.AddMilliseconds(($i + 1) * $IntervalMS)
  $remaining = [int](($target - [DateTime]::UtcNow).TotalMilliseconds)
  if ($remaining -gt 0) { Start-Sleep -Milliseconds $remaining }
}
Compress-Archive -Path (Join-Path $OutDir "frame-*.jpg") -DestinationPath $Zip -Force
Set-Content -LiteralPath ($Zip + ".done") -Value "ok"
'@
try {
  $scriptContent = $template.Replace("__MAYA_STALL_FRAME_DIR__", $outDir.Replace("\", "\\")).Replace("__MAYA_STALL_FRAME_ZIP__", $zip.Replace("\", "\\")).Replace("__MAYA_STALL_FRAME_COUNT__", "%d").Replace("__MAYA_STALL_INTERVAL_MS__", "%d")
  $scriptContent | Set-Content -Encoding ASCII -LiteralPath $script
  cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
  $startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
  $taskRun = 'powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $script + '"'
  $createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", $taskRun, "/RL", "LIMITED", "/IT", "/F")
  & schtasks.exe @createArgs | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "failed to create interactive desktop recording task with schtasks.exe /IT; ensure an interactive desktop session is logged in" }
  schtasks.exe /Run /TN $taskName | Out-Null
  $deadline = (Get-Date).AddSeconds(60)
  while ((Get-Date) -lt $deadline) {
    if ((Test-Path -LiteralPath $done) -and (Test-Path -LiteralPath $zip)) {
      $stream = [IO.File]::Open($zip, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
      try {
        $buffer = New-Object byte[] 1048576
        while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
          [Console]::OpenStandardOutput().Write($buffer, 0, $read)
        }
      } finally {
        $stream.Dispose()
      }
      exit 0
    }
    Start-Sleep -Milliseconds 250
  }
  throw "scheduled interactive desktop recording did not produce output; ensure an interactive desktop session is logged in"
} finally {
  schtasks.exe /Delete /TN $taskName /F | Out-Null
  Remove-Item -Recurse -Force -LiteralPath $root -ErrorAction SilentlyContinue
}`, powerShellSingleQuoted(remoteRoot), frames, intervalMS)
}

func extractWindowsDesktopFrameArchive(zipBytes []byte, framesDir string) error {
	reader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return fmt.Errorf("read Windows desktop frame archive: %w", err)
	}
	count := 0
	for _, file := range reader.File {
		name := filepath.Base(file.Name)
		if !strings.HasPrefix(name, "frame-") || !strings.HasSuffix(strings.ToLower(name), ".jpg") {
			continue
		}
		src, err := file.Open()
		if err != nil {
			return fmt.Errorf("open frame %s: %w", name, err)
		}
		dstPath := filepath.Join(framesDir, name)
		dst, err := os.Create(dstPath)
		if err != nil {
			_ = src.Close()
			return fmt.Errorf("create frame %s: %w", name, err)
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		_ = src.Close()
		if copyErr != nil {
			return fmt.Errorf("write frame %s: %w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close frame %s: %w", name, closeErr)
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("windows desktop frame archive contained no frames")
	}
	return nil
}
