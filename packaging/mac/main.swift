// Своё окно программы «Дима Крохобор» на Mac.
// Запускает основную программу (dimakrohobor) рядом в Contents/MacOS, получает от неё
// адрес страницы и показывает интерфейс во встроенном движке Safari (WKWebView).
// Закрыли окно или нажали Cmd+Q — основная программа тоже завершается.
import Cocoa
import WebKit

let darkBG = NSColor(srgbRed: 0x23 / 255.0, green: 0x23 / 255.0, blue: 0x23 / 255.0, alpha: 1)

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate, WKUIDelegate, WKNavigationDelegate {
    var window: NSWindow!
    var web: WKWebView!
    var server: Process?
    var pageURL: URL?
    var attached = false
    var quitting = false
    var retries = 0
    var buffer = [UInt8]()

    func applicationDidFinishLaunching(_ notification: Notification) {
        buildMenu()
        let config = WKWebViewConfiguration()
        web = WKWebView(frame: NSRect(x: 0, y: 0, width: 1400, height: 900), configuration: config)
        web.uiDelegate = self
        web.navigationDelegate = self
        web.setValue(false, forKey: "drawsBackground")

        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1400, height: 900),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "Дима Крохобор"
        window.backgroundColor = darkBG
        window.titlebarAppearsTransparent = true
        window.appearance = NSAppearance(named: .darkAqua)
        window.minSize = NSSize(width: 900, height: 620)
        window.contentView = web
        window.delegate = self
        if let vf = NSScreen.main?.visibleFrame {
            let w = min(1400, vf.width - 40), h = min(900, vf.height - 40)
            window.setFrame(NSRect(x: vf.midX - w / 2, y: vf.midY - h / 2, width: w, height: h), display: false)
        }
        window.setFrameAutosaveName("DimaKrohoborMain")
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        startServer()
    }

    // MARK: основная программа

    func startServer() {
        guard let exe = Bundle.main.url(forAuxiliaryExecutable: "dimakrohobor") else {
            fail("Не найден файл программы. Скачай «Дима Крохобор» заново.")
            return
        }
        let p = Process()
        p.executableURL = exe
        var env = ProcessInfo.processInfo.environment
        env["KROHOBOR_HOST"] = "1"
        p.environment = env
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        out.fileHandleForReading.readabilityHandler = { [weak self] h in
            let d = h.availableData
            if d.isEmpty { h.readabilityHandler = nil; return }
            DispatchQueue.main.async { self?.consume(d) }
        }
        p.terminationHandler = { [weak self] proc in
            let code = proc.terminationStatus
            DispatchQueue.main.async { self?.serverExited(code) }
        }
        do {
            try p.run()
            server = p
        } catch {
            fail("Не удалось запустить программу: \(error.localizedDescription)")
        }
    }

    func consume(_ d: Data) {
        buffer.append(contentsOf: d)
        while let nl = buffer.firstIndex(of: 10) {
            let line = String(decoding: buffer[0..<nl], as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            buffer.removeFirst(nl + 1)
            if line.hasPrefix("URL: ") {
                show(String(line.dropFirst(5)), attachedTo: false)
            } else if line.hasPrefix("ATTACH: ") {
                show(String(line.dropFirst(8)), attachedTo: true)
            }
        }
    }

    func show(_ s: String, attachedTo: Bool) {
        guard pageURL == nil, let u = URL(string: s) else { return }
        pageURL = u
        attached = attachedTo
        web.load(URLRequest(url: u))
    }

    func serverExited(_ code: Int32) {
        if quitting || attached { return }
        if pageURL == nil {
            fail("Программа не запустилась (код \(code)). Напиши в чат канала «Дима Крохобор» — поможем.")
            return
        }
        NSApp.terminate(nil)
    }

    func fail(_ msg: String) {
        quitting = true
        let a = NSAlert()
        a.messageText = "Дима Крохобор"
        a.informativeText = msg
        a.runModal()
        NSApp.terminate(nil)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        window.makeKeyAndOrderFront(nil)
        return true
    }

    func applicationWillTerminate(_ notification: Notification) {
        quitting = true
        if let p = server, p.isRunning {
            p.terminate()
            p.waitUntilExit()
        }
    }

    // MARK: страница

    func isLocal(_ u: URL) -> Bool {
        let h = (u.host ?? "").lowercased()
        return h == "127.0.0.1" || h == "localhost"
    }

    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if #available(macOS 11.3, *), navigationAction.shouldPerformDownload {
            decisionHandler(.download)
            return
        }
        if let u = navigationAction.request.url {
            let scheme = (u.scheme ?? "").lowercased()
            let inside = ["blob", "about", "data"].contains(scheme) || ((scheme == "http" || scheme == "https") && isLocal(u))
            if !inside {
                NSWorkspace.shared.open(u) // Telegram, GitHub и другие ссылки — в обычном браузере
                decisionHandler(.cancel)
                return
            }
        }
        decisionHandler(.allow)
    }

    func webView(_ webView: WKWebView, decidePolicyFor navigationResponse: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        if #available(macOS 11.3, *), !navigationResponse.canShowMIMEType {
            decisionHandler(.download)
            return
        }
        decisionHandler(.allow)
    }

    @available(macOS 11.3, *)
    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = self
    }

    @available(macOS 11.3, *)
    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        guard let u = pageURL, retries < 30 else { return }
        retries += 1
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { [weak self] in
            self?.web.load(URLRequest(url: u))
        }
    }

    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let u = navigationAction.request.url { NSWorkspace.shared.open(u) }
        return nil
    }

    func webViewDidClose(_ webView: WKWebView) { NSApp.terminate(nil) }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: "OK")
        a.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = message
        a.addButton(withTitle: "Да")
        a.addButton(withTitle: "Отмена")
        a.beginSheetModal(for: window) { r in completionHandler(r == .alertFirstButtonReturn) }
    }

    func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
        let p = NSOpenPanel()
        p.allowsMultipleSelection = parameters.allowsMultipleSelection
        p.canChooseDirectories = false
        p.canChooseFiles = true
        p.beginSheetModal(for: window) { r in completionHandler(r == .OK ? p.urls : nil) }
    }

    // MARK: меню

    @objc func reloadPage(_ sender: Any?) { web.reload() }

    func buildMenu() {
        let main = NSMenu()

        let appItem = NSMenuItem()
        main.addItem(appItem)
        let app = NSMenu()
        app.addItem(withTitle: "О программе «Дима Крохобор»", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        app.addItem(NSMenuItem.separator())
        app.addItem(withTitle: "Скрыть «Дима Крохобор»", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        let others = app.addItem(withTitle: "Скрыть остальные", action: #selector(NSApplication.hideOtherApplications(_:)), keyEquivalent: "h")
        others.keyEquivalentModifierMask = [.command, .option]
        app.addItem(withTitle: "Показать все", action: #selector(NSApplication.unhideAllApplications(_:)), keyEquivalent: "")
        app.addItem(NSMenuItem.separator())
        app.addItem(withTitle: "Завершить «Дима Крохобор»", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = app

        let editItem = NSMenuItem()
        main.addItem(editItem)
        let edit = NSMenu(title: "Правка")
        edit.addItem(withTitle: "Отменить", action: Selector(("undo:")), keyEquivalent: "z")
        let redo = edit.addItem(withTitle: "Повторить", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(NSMenuItem.separator())
        edit.addItem(withTitle: "Вырезать", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Копировать", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Вставить", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Выбрать все", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit

        let viewItem = NSMenuItem()
        main.addItem(viewItem)
        let view = NSMenu(title: "Вид")
        let reload = view.addItem(withTitle: "Обновить", action: #selector(reloadPage(_:)), keyEquivalent: "r")
        reload.target = self
        let full = view.addItem(withTitle: "Во весь экран", action: #selector(NSWindow.toggleFullScreen(_:)), keyEquivalent: "f")
        full.keyEquivalentModifierMask = [.command, .control]
        viewItem.submenu = view

        let winItem = NSMenuItem()
        main.addItem(winItem)
        let win = NSMenu(title: "Окно")
        win.addItem(withTitle: "Свернуть", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        win.addItem(withTitle: "Закрыть", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        winItem.submenu = win

        NSApp.mainMenu = main
        NSApp.windowsMenu = win
    }
}

@available(macOS 11.3, *)
extension AppDelegate: WKDownloadDelegate {
    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse,
                  suggestedFilename: String, completionHandler: @escaping (URL?) -> Void) {
        let p = NSSavePanel()
        p.nameFieldStringValue = suggestedFilename
        p.directoryURL = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
        p.beginSheetModal(for: window) { r in
            guard r == .OK, let u = p.url else { completionHandler(nil); return }
            try? FileManager.default.removeItem(at: u)
            completionHandler(u)
        }
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
