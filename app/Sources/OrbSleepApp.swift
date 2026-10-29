import SwiftUI

@main
struct OrbSleepApp: App {
    @State private var appState = AppState()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(appState)
                .frame(minWidth: 1100, minHeight: 700)
        }
        .defaultSize(width: 1280, height: 840)
        .windowToolbarStyle(.unified)
        .commands {
            CommandGroup(replacing: .newItem) {}
        }
    }
}

// MARK: - 根视图

struct RootView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        @Bindable var app = app
        NavigationSplitView {
            SidebarView()
                .navigationSplitViewColumnWidth(min: 200, ideal: 220, max: 300)
        } detail: {
            detailContent
                .frame(minWidth: 760, maxWidth: .infinity, maxHeight: .infinity)
                .sheet(item: $app.plan) { plan in
                    PlanSheetView(plan: plan)
                }
        }
        .navigationSplitViewStyle(.balanced)
        .sheet(isPresented: $app.isOperationSheetPresented) {
            OperationSheetView()
        }
        .overlay(alignment: .top) {
            if let message = app.errorBanner {
                ErrorBanner(message: message) {
                    app.dismissError()
                }
                .padding(.horizontal, 24)
                .padding(.top, 10)
                .shadow(color: .black.opacity(0.12), radius: 6, y: 2)
                .transition(.move(edge: .top).combined(with: .opacity))
            }
        }
        .animation(.easeInOut(duration: 0.18), value: app.errorBanner)
        .task {
            await app.bootstrap()
        }
        .frame(minWidth: 1100, minHeight: 700)
    }

    @ViewBuilder
    private var detailContent: some View {
        if !app.daemonReachable {
            DaemonUnavailableView()
        } else {
            switch app.section {
            case .overview:
                OverviewView()
            case .scan:
                ScanView()
            case .services:
                ServicesView()
            case .operations:
                OperationsView()
            case .settings:
                SettingsView()
            }
        }
    }
}

// MARK: - 侧边栏

struct SidebarView: View {
    @Environment(AppState.self) private var app

    var body: some View {
        List(selection: selectionBinding) {
            Section("导航") {
                ForEach(AppSection.allCases) { section in
                    if let count = badge(for: section) {
                        Label(section.title, systemImage: section.systemImage)
                            .tag(section)
                            .badge(count)
                    } else {
                        Label(section.title, systemImage: section.systemImage)
                            .tag(section)
                    }
                }
            }

            Section("后台助手") {
                VStack(alignment: .leading, spacing: 6) {
                    HStack(spacing: 6) {
                        Circle()
                            .fill(app.daemonReachable ? Color.green : Color.red)
                            .frame(width: 8, height: 8)
                        Text(app.daemonReachable ? "已连接 orbsleepd" : "未连接")
                            .font(.callout)
                    }
                    if let version = app.status?.daemonVersion, !version.isEmpty {
                        Text("版本 \(version)")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    if let name = app.status?.context?.name, !name.isEmpty {
                        Text("上下文 \(name)")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    Text("Socket：\(app.socketPath)")
                        .font(.caption2.monospaced())
                        .foregroundStyle(.tertiary)
                        .lineLimit(2)
                        .truncationMode(.middle)
                }
                .padding(.vertical, 2)
            }
        }
        .listStyle(.sidebar)
    }

    private var selectionBinding: Binding<AppSection?> {
        Binding(
            get: { app.section },
            set: { newValue in
                if let newValue {
                    app.section = newValue
                }
            }
        )
    }

    private func badge(for section: AppSection) -> Int? {
        switch section {
        case .scan:
            let count = app.pendingReviewCount
            return count > 0 ? count : nil
        case .services:
            let count = app.status?.counts?.managed ?? app.services.count
            return count > 0 ? count : nil
        default:
            return nil
        }
    }
}
