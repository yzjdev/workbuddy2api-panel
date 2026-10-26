// main.go — workbuddy2api 桌面控制台（Windows 原生窗口，walk 实现）。
//
// 窗口三块：状态指示（运行中/已停止）+ 启动/停止按钮 + 接口地址与 API Key 展示
// （一键复制）。以子进程方式拉起/终止同目录的 workbuddy2api-panel.exe
// （CREATE_NO_WINDOW 不闪黑框），日志落在同目录 server.log。
//
// 构建：需要 common controls v6 manifest（rsrc -manifest 生成 .syso），
// 见 gui.manifest；无 manifest 时窗口仍可用但控件样式为旧版。
// 双击运行不弹终端：以 windowsgui 子系统链接——
//
//	go build -ldflags "-H windowsgui" -o workbuddy2api-gui.exe ./cmd/gui
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	. "github.com/lxn/walk/declarative"
)

const serverBin = "workbuddy2api-panel.exe"

// Job Object：把网关进程绑定到 GUI 的生命周期。
// 点 X 关窗时 Closing 会调 stop() 正常停止网关；但如果 GUI 被任务管理器/崩溃
// 强杀，Closing 根本不触发，网关就成孤儿进程继续占着 7863——Web 面板看着还活着。
// 把网关加入「关闭 job 句柄即全杀」的 job 后，GUI 无论以何种方式退出，Windows
// 都会随句柄关闭结束 job 内所有进程，这是强杀场景唯一可靠的兜底。
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
)

type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimit struct {
	BasicLimitInformation jobBasicLimit
	IoInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// newKillOnCloseJob 创建「GUI 退出即杀掉组内进程」的 job；失败返回 0（降级为
// 只靠 Closing 清理，点 X 关闭仍然有效，只是强杀场景无兜底）。
func newKillOnCloseJob() syscall.Handle {
	h, _, _ := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0
	}
	info := jobExtendedLimit{BasicLimitInformation: jobBasicLimit{LimitFlags: jobObjectLimitKillOnJobClose}}
	if r, _, _ := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return 0
	}
	return syscall.Handle(h)
}

// assignToJob 把 pid 加入 job（失败忽略：job 不可用或进程已退出时不影响主流程）。
func assignToJob(job syscall.Handle, pid int) {
	if job == 0 || pid <= 0 {
		return
	}
	const processSetQuota, processTerminate = 0x0100, 0x0001
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)
	_, _, _ = procAssignProcessToJobObject.Call(uintptr(job), uintptr(h))
}

type controller struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	job    syscall.Handle
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

// killServerByImage 结束所有名为 serverBin 的进程（可能由上一次 GUI、命令行或
// 机器开机脚本启动）。用于关闭窗口时的兜底清理：外部启动的进程不在 c.cmd 里，
// 只能按映像名清理。
func killServerByImage() {
	cmd := exec.Command("taskkill", "/IM", serverBin, "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = cmd.Run() // 没有匹配进程时 taskkill 返回非 0，忽略
	// 等端口释放（taskkill 返回时进程可能仍在退出中）。
	time.Sleep(300 * time.Millisecond)
}

// serverAddr 把 listen（形如 ":7863" / "0.0.0.0:7863"）换算成探活用的 host:port。
func serverAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	if strings.HasPrefix(listen, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(listen, "0.0.0.0:")
	}
	return listen
}

// lanIP 探测本机局域网 IPv4（第一个非 loopback、非链路本地的地址），找不到返回空串。
func lanIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

// serverProcessExists 报告网关映像是否在运行（可能由上次 GUI 或外部启动）。
func serverProcessExists() bool {
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq "+serverBin, "/NH")
	// 与控制台子进程同样隐藏窗口：否则每次探活都会闪一个终端黑框。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), strings.ToLower(serverBin))
}

// externalAlive 报告是否已有网关实例在服务。
// 先用端口快探（120ms，已监听的实例通常 1ms 内返回），只在端口不通时才用
// tasklist 兜底（进程存在但还没开始监听/卡住的场景）——tasklist 要起子进程，
// 约 100~300ms，放在热路径上会明显拖慢启动。
func (c *controller) externalAlive() bool {
	if conn, err := net.DialTimeout("tcp", serverAddr(c.listen), 120*time.Millisecond); err == nil {
		_ = conn.Close()
		return true
	}
	return serverProcessExists()
}

func (c *controller) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil && c.cmd.ProcessState == nil {
		return nil
	}
	// 已有实例则复用：不杀不启，窗口直接显示「运行中」。
	if c.externalAlive() {
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
	// 加入「句柄关闭即全杀」的 job：GUI 被强杀时也能随句柄关闭带走网关。
	assignToJob(c.job, cmd.Process.Pid)
	c.cmd = cmd
	go func() { _ = cmd.Wait() }()
	return nil
}

func (c *controller) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil && c.cmd.ProcessState == nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	c.cmd = nil
	// 关闭即停止：复用的外部实例 / 遗留进程一并结束（按映像名兜底）。
	killServerByImage()
}

// childRunning 报告本 GUI 拉起的网关子进程是否存活（不做任何探活，微秒级）。
func (c *controller) childRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd != nil && c.cmd.Process != nil && c.cmd.ProcessState == nil
}

// running 报告网关是否在服务：本 GUI 拉起的子进程存活，或已复用的外部实例存活。
func (c *controller) running() bool {
	if c.childRunning() {
		return true
	}
	return c.externalAlive()
}

// killPrevInstances 结束本映像名的其它 GUI 实例（双击再启动时先关掉上一个窗口）。
// 旧实例退出时其 Job Object 会连带停掉它启动的网关，由新实例接管。
// 不能直接 taskkill /IM —— 那会把新实例自己也杀掉，故先按 PID 过滤。
func killPrevInstances() {
	self := os.Getpid()
	cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("IMAGENAME eq %s", filepath.Base(selfExe())), "/NH")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid == self {
			continue
		}
		kill := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F")
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		_ = kill.Run()
	}
	// 给旧实例一点退出时间（其网关随之退出、端口释放）。
	time.Sleep(500 * time.Millisecond)
}

// selfExe 返回当前可执行文件完整路径；取不到时返回空串。
func selfExe() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

func main() {
	// 双击再启动：先关掉上一个 GUI 实例（其网关随 Job Object 退出，由本实例接管）。
	killPrevInstances()
	dir, _ := os.Getwd()
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	listen, apiKey := loadConfig(dir)
	ctl := &controller{cfgDir: dir, listen: listen, apiKey: apiKey, job: newKillOnCloseJob()}
	// 复制用完整地址；界面显示去掉 scheme（标签文本决定布局最小宽度，
	// 太长会把窗口撑宽——walk 会把窗口扩张到内容最小宽，最小化恢复时尤其明显）。
	urlShort := "127.0.0.1" + listen + "/v1"
	urlFull := "http://" + urlShort
	// 局域网地址：仅当服务监听所有网卡（:port / 0.0.0.0:port）时才可从局域网访问。
	lanHost := lanIP()
	lanReachable := strings.HasPrefix(listen, ":") || strings.HasPrefix(listen, "0.0.0.0:")
	lanShort := ""
	lanFull := ""
	if lanHost != "" && lanReachable {
		lanShort = lanHost + listen + "/v1"
		lanFull = "http://" + lanShort
	}

	var mw *walk.MainWindow
	var dot *walk.Label
	var urlLbl, keyLbl, lanLbl *walk.Label
	var btnCopyURL, btnCopyKey, btnCopyLAN *walk.PushButton
	// 无局域网地址（探测失败）或仅绑定本机回环时给出说明文案。
	lanText := "不可用（仅本机回环监听）"
	if lanShort != "" {
		lanText = lanShort
	} else if lanHost == "" {
		lanText = "未检测到局域网网卡"
	}

	// 状态点颜色：walk 的 WM_CTLCOLORSTATIC 由 ContainerBase 处理，而状态点是
	// 主窗口的直接子控件，FormBase 不处理该消息，Label.SetTextColor / 声明式
	// TextColor 都不会生效。真正应用颜色的地方是下方子类化窗口过程的
	// WM_CTLCOLORSTATIC 分支（dotHwnd 比对 + SetTextColor + HOLLOW_BRUSH），
	// 这里只改变量值。
	var dotColor win.COLORREF = win.COLORREF(win.RGB(220, 38, 38)) // 初始：已停止=红
	setRunning := func(run bool) {
		if run {
			dotColor = win.COLORREF(win.RGB(22, 163, 74)) // 绿
			dot.SetText("● 运行中")
		} else {
			dotColor = win.COLORREF(win.RGB(220, 38, 38)) // 红
			dot.SetText("● 已停止")
		}
	}

	err := MainWindow{
		AssignTo: &mw,
		Title:    "WB2API",
		// 尺寸自适应：不写死任何宽高。1x1 只是创建时的种子——不给 Bounds 时
		// walk 会走 CW_USEDEFAULT 得到一个大窗口，给个小种子后布局引擎首轮
		// (startLayout) 会按内容最小尺寸自动扩张，窗口大小完全由控件决定。
		// 坐标放到屏幕外（-10000,-10000）：窗口即刻可见→布局立即运行（隐藏窗口
		// 时布局不一定跑，要等超时兜底，启动就会慢 1.5s），同时用户看不到左上角
		// 闪现；首轮尺寸采样完成后 centerOnScreen 把它拉到屏幕中央。
		Bounds: Rectangle{X: -10000, Y: -10000, Width: 1, Height: 1},
		Layout: VBox{Margins: Margins{Left: 8, Top: 6, Right: 8, Bottom: 6}},
		Children: []Widget{
			Label{AssignTo: &dot, Text: "● 已停止", TextColor: walk.RGB(220, 38, 38), Font: Font{PointSize: 10}},
			VSpacer{Size: 2},
			Composite{
				Layout: Grid{Columns: 3, Spacing: 4},
				Children: []Widget{
					// 显示用短文本（布局最小宽度由此决定）；复制走完整值。
					Label{Text: "地址:"},
					Label{AssignTo: &urlLbl, Text: urlShort, Font: Font{PointSize: 8, Family: "Consolas"}},
					PushButton{AssignTo: &btnCopyURL, Text: "复制", MinSize: Size{Width: 44, Height: 20}, OnClicked: func() {
						walk.Clipboard().SetText(urlFull)
						flashCopied(btnCopyURL)
					}},
					Label{Text: "局域网:"},
					Label{AssignTo: &lanLbl, Text: lanText, Font: Font{PointSize: 8, Family: "Consolas"}},
					PushButton{AssignTo: &btnCopyLAN, Text: "复制", MinSize: Size{Width: 44, Height: 20}, OnClicked: func() {
						if lanFull != "" {
							walk.Clipboard().SetText(lanFull)
						}
						flashCopied(btnCopyLAN)
					}},
					Label{Text: "Key:"},
					Label{AssignTo: &keyLbl, Text: maskKey(apiKey), Font: Font{PointSize: 8, Family: "Consolas"}},
					PushButton{AssignTo: &btnCopyKey, Text: "复制", MinSize: Size{Width: 44, Height: 20}, OnClicked: func() {
						walk.Clipboard().SetText(apiKey)
						flashCopied(btnCopyKey)
					}},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					PushButton{Text: "打开面板", MinSize: Size{Height: 20}, OnClicked: func() {
						_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", "http://127.0.0.1"+listen+"/panel/").Start()
					}},
				},
			},
		},
	}.Create()
	if err != nil {
		showError(err)
		return
	}
	// 尺寸锁：不写死宽高（内容自适应），但最小化恢复时 walk 会把 proposedSize
	// 记成极小值（实测 190x32），恢复时按它重算布局最小宽度，把窗口撑宽——且
	// walk 的 SizeChanged 事件在恢复路径并不触发，事件式压回无效。这里直接
	// 子类化窗口过程：在 WM_WINDOWPOSCHANGING（尺寸生效前）把任何偏离锁定值的
	// 尺寸改写回锁定值，窗口因此永远保持首轮布局算出的内容尺寸。
	var locked walk.Size
	var lockMu sync.Mutex
	// showOnce 保证「居中 + 显示」只执行一次：正常由首轮尺寸采样触发，
	// 超时兜底或异常路径也不会重复显示/重复居中。
	var showOnce sync.Once
	showWindow := func() {
		mw.Synchronize(func() {
			showOnce.Do(func() {
				centerOnScreen(mw)
				mw.SetVisible(true)
			})
		})
	}
	var prevProc uintptr
	// 状态点着色：walk 的 Label 是两层窗口——外层 Walk_Static_Class（dot.Handle()）
	// 里嵌一个原生 static 子窗口负责绘制文本。WM_CTLCOLORSTATIC 由子窗口发给
	// 它的父窗口，也就是**Label 自己的外层窗口**；挂主窗口/布局容器都收不到。
	// 且消息若落到 DefWindowProc 会把文本色重置回系统黑，所以命中后必须返回
	// 非零画刷截断默认处理。
	// 画刷必须实心（用窗口底色 COLOR_BTNFACE）且背景模式为 OPAQUE：
	// 之前用 HOLLOW_BRUSH + TRANSPARENT 时 static 不擦背景，"已停止"切到
	// "运行中" 会留下旧字残影，看起来就是文案显示异常。
	dotBg := win.COLORREF(win.GetSysColor(win.COLOR_BTNFACE))
	if dot != nil {
		var colorPrevProc uintptr
		colorPrevProc = win.SetWindowLongPtr(dot.Handle(), win.GWL_WNDPROC, syscall.NewCallback(
			func(h win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
				if msg == win.WM_CTLCOLORSTATIC {
					hdc := win.HDC(wParam)
					win.SetTextColor(hdc, dotColor)
					win.SetBkColor(hdc, dotBg)
					win.SetBkMode(hdc, win.OPAQUE)
					return uintptr(win.GetSysColorBrush(win.COLOR_BTNFACE))
				}
				return win.CallWindowProc(colorPrevProc, h, msg, wParam, lParam)
			}))
	}
	prevProc = win.SetWindowLongPtr(mw.Handle(), win.GWL_WNDPROC, syscall.NewCallback(
		func(h win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
			switch msg {
			case win.WM_WINDOWPOSCHANGING:
				wp := (*win.WINDOWPOS)(unsafe.Pointer(lParam))
				lockMu.Lock()
				lk := locked
				// 采样必须在 CHANGING 阶段：CHANGED 在恢复路径已被 walk 撑大。
				sampled := false
				if lk.Width == 0 && wp.Flags&win.SWP_NOSIZE == 0 &&
					!win.IsIconic(h) && wp.Cx > 200 && wp.Cy > 100 {
					lk = walk.Size{Width: int(wp.Cx), Height: int(wp.Cy)}
					locked = lk
					sampled = true
				}
				lockMu.Unlock()
				if lk.Width > 0 && wp.Flags&win.SWP_NOSIZE == 0 {
					wp.Cx, wp.Cy = int32(lk.Width), int32(lk.Height)
				}
				if sampled {
					// 锁定首轮内容尺寸后居中并显示：延迟到消息栈外执行，
					// 避免在 WndProc 内重入 SetWindowPos 改变尺寸。
					time.AfterFunc(0, showWindow)
				}
			case win.WM_WINDOWPOSCHANGED:
				// 先让原窗口过程跑完（walk 会在这里把窗口撑到它算出的尺寸），
				// 随后按锁定尺寸校正。校正走完整 SetWindowPos：会先经过上面的
				// CHANGING 钩子（此时尺寸已等于锁定值，不会二次触发）。
				ret := win.CallWindowProc(prevProc, h, msg, wParam, lParam)
				lockMu.Lock()
				lk := locked
				lockMu.Unlock()
				if lk.Width > 0 && !win.IsIconic(h) {
					var rc win.RECT
					if win.GetWindowRect(h, &rc) {
						if int(rc.Right-rc.Left) != lk.Width || int(rc.Bottom-rc.Top) != lk.Height {
							win.SetWindowPos(h, 0, 0, 0, int32(lk.Width), int32(lk.Height),
								win.SWP_NOMOVE|win.SWP_NOZORDER|win.SWP_NOACTIVATE)
						}
					}
				}
				return ret
			}
			return win.CallWindowProc(prevProc, h, msg, wParam, lParam)
		}))

	// 关窗即停服务：控制台就是服务的生命周期。
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		ctl.stop()
	})
	// 打开即启动：已存在的网关进程由外部实例复用，不再重启。
	go func() {
		err := ctl.start()
		if err != nil {
			mw.Synchronize(func() {
				walk.MsgBox(mw, "启动失败", err.Error(), walk.MsgBoxIconError)
				setRunning(false)
			})
			return
		}
		// 启动/复用成功都是运行中：直接置位，省掉一次端口探活 + tasklist（约 300ms）。
		mw.Synchronize(func() { setRunning(true) })
	}()
	// 超时兜底：布局异常等情况下若始终没采样到首轮尺寸，也要把窗口居中显示，
	// 避免「窗口永远不会出现」。正常路径在布局跑完（<100ms）就居中拉回，
	// 这里只兜异常，故取较短等待。showOnce 保证不会重复显示。
	go func() {
		time.Sleep(600 * time.Millisecond)
		showWindow()
	}()
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
