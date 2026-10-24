// main.go — workbuddy2api 桌面控制台（Windows 原生窗口，walk 实现）。
//
// 窗口三块：状态指示（运行中/已停止）+ 启动/停止按钮 + 接口地址与 API Key 展示
// （一键复制）。以子进程方式拉起/终止同目录的 workbuddy2api-panel.exe
// （CREATE_NO_WINDOW 不闪黑框），日志落在同目录 server.log。
//
// 构建：需要 common controls v6 manifest（rsrc -manifest 生成 .syso），
// 见 gui.manifest；无 manifest 时窗口仍可用但控件样式为旧版。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

const serverBin = "workbuddy2api-panel.exe"

type controller struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	cfgDir string
	listen string
	apiKey string
}

func loadConfig(dir string) (listen, apiKey string) {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ":7863", ""
	}
	var c struct {
		Listen string `json:"listen"`
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(raw, &c) == nil && c.Listen != "" {
		return c.Listen, c.APIKey
	}
	return ":7863", ""
}

func (c *controller) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil && c.cmd.ProcessState == nil {
		return nil
	}
	cmd := exec.Command(filepath.Join(c.cfgDir, serverBin))
	cmd.Dir = c.cfgDir
	// CREATE_NO_WINDOW=0x08000000：不闪控制台黑框。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	logFile, err := os.OpenFile(filepath.Join(c.cfgDir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		cmd.Stdout, cmd.Stderr = logFile, logFile
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", serverBin, err)
	}
	c.cmd = cmd
	go func() { _ = cmd.Wait() }()
	return nil
}

func (c *controller) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil || c.cmd.ProcessState != nil {
		c.cmd = nil
		return
	}
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
	c.cmd = nil
}

func (c *controller) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil && c.cmd.Process != nil && c.cmd.ProcessState == nil
}

func main() {
	dir, _ := os.Getwd()
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	listen, apiKey := loadConfig(dir)
	ctl := &controller{cfgDir: dir, listen: listen, apiKey: apiKey}

	var mw *walk.MainWindow
	var dot *walk.Label
	var urlLbl, keyLbl *walk.Label
	var btnStart, btnStop, btnCopyURL, btnCopyKey *walk.PushButton

	refresh := func() {
		run := ctl.running()
		if run {
			dot.SetText("● 运行中")
			dot.SetTextColor(walk.RGB(22, 163, 74))
		} else {
			dot.SetText("● 已停止")
			dot.SetTextColor(walk.RGB(220, 38, 38))
		}
		btnStart.SetEnabled(!run)
		btnStop.SetEnabled(run)
	}

	err := MainWindow{
		AssignTo: &mw,
		Title:    "workbuddy2api",
		// 极小初始 Bounds：CW_USEDEFAULT 会给出很大的默认窗口；给 1x1 后 walk
		// 布局（startLayout）会自动把窗口扩张到内容的最小宽高，即真正的自适应。
		Bounds: Rectangle{Width: 1, Height: 1},
		Layout: VBox{Margins: Margins{Left: 12, Top: 10, Right: 12, Bottom: 10}},
		Children: []Widget{
			Label{AssignTo: &dot, Text: "● 已停止", Font: Font{PointSize: 11}},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 10},
				Children: []Widget{
					PushButton{AssignTo: &btnStart, Text: "启动", MinSize: Size{Width: 70, Height: 24}},
					PushButton{AssignTo: &btnStop, Text: "停止", MinSize: Size{Width: 70, Height: 24}},
				},
			},
			VSpacer{Size: 3},
			Composite{
				Layout: Grid{Columns: 3, Spacing: 6},
				Children: []Widget{
					Label{Text: "地址:"},
					Label{AssignTo: &urlLbl, Text: "http://127.0.0.1" + listen + "/v1", Font: Font{PointSize: 9, Family: "Consolas"}},
					PushButton{AssignTo: &btnCopyURL, Text: "复制", MinSize: Size{Width: 52, Height: 22}, OnClicked: func() {
						walk.Clipboard().SetText(urlLbl.Text())
						flashCopied(btnCopyURL)
					}},
					Label{Text: "Key:"},
					Label{AssignTo: &keyLbl, Text: maskKey(apiKey), Font: Font{PointSize: 9, Family: "Consolas"}},
					PushButton{AssignTo: &btnCopyKey, Text: "复制", MinSize: Size{Width: 52, Height: 22}, OnClicked: func() {
						walk.Clipboard().SetText(apiKey)
						flashCopied(btnCopyKey)
					}},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					PushButton{Text: "打开面板", MinSize: Size{Height: 22}, OnClicked: func() {
						_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", "http://127.0.0.1"+listen+"/panel/").Start()
					}},
				},
			},
			VSpacer{Size: 2},
		},
	}.Create()
	if err != nil {
		showError(err)
		return
	}
	// 尺寸自适应：不设 Min/MaxSize，由布局按内容算出最小宽高；Create 后按屏幕居中。
	centerOnScreen(mw)

	btnStart.Clicked().Attach(func() {
		btnStart.SetEnabled(false)
		go func() {
			err := ctl.start()
			time.Sleep(400 * time.Millisecond)
			mw.Synchronize(func() {
				if err != nil {
					walk.MsgBox(mw, "启动失败", err.Error(), walk.MsgBoxIconError)
				}
				refresh()
			})
		}()
	})
	btnStop.Clicked().Attach(func() {
		btnStop.SetEnabled(false)
		go func() {
			ctl.stop()
			mw.Synchronize(refresh)
		}()
	})

	refresh()
	// 关窗即停服务：控制台就是服务的生命周期。
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		ctl.stop()
	})
	mw.Run()
}

// flashCopied 复制成功提示：按钮文案短暂变为「已复制」后还原。
// 必须在 UI 线程调用（按钮 OnClicked 即 UI 线程）；多次连点以最后一次为准。
func flashCopied(b *walk.PushButton) {
	old := b.Text()
	b.SetText("已复制")
	time.AfterFunc(1200*time.Millisecond, func() {
		// AfterFunc 的回调在独立 goroutine，回 UI 线程改控件。
		b.Parent().Synchronize(func() {
			if b.Text() == "已复制" { // 期间用户又复制过则不覆盖新的提示
				b.SetText(old)
			}
		})
	})
}

func showError(err error) {
	_ = walk.MsgBox(nil, "错误", err.Error(), walk.MsgBoxIconError)
}

// maskKey 打码展示 API Key：前 4 + **** + 后 4；过短全打码；空 = 未启用鉴权。
func maskKey(k string) string {
	if k == "" {
		return "（未启用鉴权）"
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

// centerOnScreen 把窗口移到主屏幕中央（去掉任务栏的工作区用 SPI_GETWORKAREA，
// 失败退回全屏尺寸）。Create 后调用一次即可。
func centerOnScreen(w walk.Form) {
	type rect struct{ L, T, R, B int32 }
	const SPI_GETWORKAREA = 0x0030
	var rc rect
	r1, _, _ := syscall.NewLazyDLL("user32.dll").NewProc("SystemParametersInfoW").Call(
		uintptr(SPI_GETWORKAREA), 0, uintptr(unsafe.Pointer(&rc)), 0)
	sw, sh := int(rc.R-rc.L), int(rc.B-rc.T)
	if r1 == 0 || sw <= 0 || sh <= 0 {
		const SM_CXSCREEN, SM_CYSCREEN = 0, 1
		gsm := syscall.NewLazyDLL("user32.dll").NewProc("GetSystemMetrics")
		cx, _, _ := gsm.Call(SM_CXSCREEN)
		cy, _, _ := gsm.Call(SM_CYSCREEN)
		sw, sh = int(cx), int(cy)
		if sw <= 0 || sh <= 0 {
			return
		}
	}
	b := w.BoundsPixels()
	x, y := (sw-b.Width)/2, (sh-b.Height)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	_ = w.SetBoundsPixels(walk.Rectangle{X: x, Y: y, Width: b.Width, Height: b.Height})
}
