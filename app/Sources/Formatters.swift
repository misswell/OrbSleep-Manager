import Foundation

/// 时间展示：协议里所有时间都是 RFC 3339（UTC，带 `Z`）。
enum RelativeTime {
    static func date(from text: String?) -> Date? {
        guard let text, !text.isEmpty else { return nil }
        if let parsed = try? Date.ISO8601FormatStyle(includingFractionalSeconds: true).parse(text) {
            return parsed
        }
        if let parsed = try? Date.ISO8601FormatStyle(includingFractionalSeconds: false).parse(text) {
            return parsed
        }
        return nil
    }

    /// 相对时间，例如「3 分钟前」。`nil` 或无法解析时返回占位文案。
    static func text(_ raw: String?, placeholder: String = "从未") -> String {
        guard let date = date(from: raw) else { return placeholder }
        let interval = Date().timeIntervalSince(date)
        if interval < 0 { return "即将" }
        if interval < 10 { return "刚刚" }
        if interval < 60 { return "\(Int(interval)) 秒前" }
        if interval < 3600 { return "\(Int(interval / 60)) 分钟前" }
        if interval < 86_400 { return "\(Int(interval / 3600)) 小时前" }
        if interval < 86_400 * 30 { return "\(Int(interval / 86_400)) 天前" }
        return absolute(date)
    }

    static func absolute(_ date: Date) -> String {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "zh_CN")
        formatter.dateFormat = "yyyy-MM-dd HH:mm"
        return formatter.string(from: date)
    }

    /// 日志时间（HH:mm:ss）。
    static func clock(_ raw: String?) -> String {
        guard let date = date(from: raw) else { return "--:--:--" }
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "HH:mm:ss"
        return formatter.string(from: date)
    }
}

enum DurationText {
    /// 秒数转中文时长。
    static func short(_ seconds: Int?) -> String {
        guard let seconds, seconds > 0 else { return "—" }
        if seconds < 60 { return "\(seconds) 秒" }
        if seconds < 3600 {
            let minutes = seconds / 60
            let rest = seconds % 60
            return rest == 0 ? "\(minutes) 分钟" : "\(minutes) 分 \(rest) 秒"
        }
        let hours = seconds / 3600
        let minutes = (seconds % 3600) / 60
        return minutes == 0 ? "\(hours) 小时" : "\(hours) 小时 \(minutes) 分"
    }

    /// 两个时间点之间的耗时。
    static func between(start: String?, end: String?) -> String? {
        guard let startDate = RelativeTime.date(from: start) else { return nil }
        let endDate = RelativeTime.date(from: end) ?? Date()
        let seconds = Int(max(0, endDate.timeIntervalSince(startDate)))
        if seconds < 1 { return "不到 1 秒" }
        return short(seconds)
    }
}
