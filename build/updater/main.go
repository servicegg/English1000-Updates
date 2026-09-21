//go:build windows

package main

import (
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "io"
    "os"
    "os/exec"
    "path/filepath"
    "strconv"
    "strings"
    "syscall"
    "time"
    "unsafe"
)

const synchronize = 0x00100000

func logLine(path, s string) {
    if path == "" { return }
    f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
    if err != nil { return }
    defer f.Close()
    fmt.Fprintf(f, "%s | updater | %s\r\n", time.Now().Format("2006-01-02 15:04:05.000"), s)
}

func messageBox(title, body string) {
    user32 := syscall.NewLazyDLL("user32.dll")
    proc := user32.NewProc("MessageBoxW")
    t, _ := syscall.UTF16PtrFromString(body)
    c, _ := syscall.UTF16PtrFromString(title)
    proc.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x10)
}

func arg(name string) string {
    prefix := "--" + name + "="
    for _, a := range os.Args[1:] {
        if strings.HasPrefix(a, prefix) { return strings.TrimPrefix(a, prefix) }
    }
    return ""
}

func waitForPID(pid int, timeout time.Duration) {
    if pid <= 0 { return }
    kernel32 := syscall.NewLazyDLL("kernel32.dll")
    openProcess := kernel32.NewProc("OpenProcess")
    waitSingle := kernel32.NewProc("WaitForSingleObject")
    closeHandle := kernel32.NewProc("CloseHandle")
    h, _, _ := openProcess.Call(synchronize, 0, uintptr(pid))
    if h == 0 { time.Sleep(400*time.Millisecond); return }
    defer closeHandle.Call(h)
    waitSingle.Call(h, uintptr(timeout/time.Millisecond))
}

func fileSHA256(path string) (string, error) {
    f, err := os.Open(path)
    if err != nil { return "", err }
    defer f.Close()
    h := sha256.New()
    if _, err := io.Copy(h, f); err != nil { return "", err }
    return hex.EncodeToString(h.Sum(nil)), nil
}

func replaceWithRetry(source, target, logPath string) error {
    backup := target + ".old"
    _ = os.Remove(backup)
    var last error
    for i := 0; i < 80; i++ {
        if _, err := os.Stat(target); err == nil {
            if err := os.Rename(target, backup); err != nil {
                last = err
                time.Sleep(125*time.Millisecond)
                continue
            }
        }
        if err := os.Rename(source, target); err == nil {
            _ = os.Remove(backup)
            logLine(logPath, "core replacement complete")
            return nil
        } else {
            last = err
            if _, statErr := os.Stat(backup); statErr == nil { _ = os.Rename(backup, target) }
            time.Sleep(125*time.Millisecond)
        }
    }
    return last
}

func main() {
    source := arg("source")
    target := arg("target")
    expected := strings.ToLower(strings.TrimSpace(arg("sha256")))
    logPath := arg("log")
    pid, _ := strconv.Atoi(arg("pid"))
    if source == "" || target == "" || len(expected) != 64 { return }

    waitForPID(pid, 15*time.Second)
    got, err := fileSHA256(source)
    if err != nil || !strings.EqualFold(got, expected) {
        logLine(logPath, "staged core rejected: sha256 mismatch")
        _ = os.Remove(source)
        return
    }

    if err := replaceWithRetry(source, target, logPath); err != nil {
        logLine(logPath, "core replacement failed: "+err.Error())
        messageBox("English 1000", "Не удалось установить обновление ядра приложения. Старая версия сохранена.")
        return
    }

    time.Sleep(350*time.Millisecond)
    cmd := exec.Command(target)
    cmd.Dir = filepath.Dir(target)
    cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow:true}
    if err := cmd.Start(); err != nil {
        logLine(logPath, "restart failed: "+err.Error())
        messageBox("English 1000", "Обновление установлено, но приложение не удалось запустить автоматически.")
    }
}
