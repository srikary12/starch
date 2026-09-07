import Foundation
import Testing

@testable import StarchKit

@Suite("Services menu state")
struct ServicesMenuTests {
    /// The exact key macOS wrote for our service, captured from a real
    /// `defaults read pbs NSServicesStatus`. If this drifts, detection
    /// silently reports "not configured" forever.
    private let realKey = "dev.starch.Starch - Starch - rewriteSelection"

    @Test("the status key matches what pbs actually writes")
    func keyFormat() {
        #expect(ServicesMenu.statusKey() == realKey)
    }

    /// The menu item title is part of the key, so renaming the entry in
    /// NSServices resets the user's choice and hands them a fresh unticked
    /// service with no explanation. Worth having a test that says so.
    @Test("the key is sensitive to the menu title and the message")
    func keyDependsOnTitleAndMessage() {
        #expect(
            ServicesMenu.statusKey(menuItemTitle: "Polish")
                == "dev.starch.Starch - Polish - rewriteSelection"
        )
        #expect(
            ServicesMenu.statusKey(message: "polish")
                == "dev.starch.Starch - Starch - polish"
        )
    }

    @Test("a fresh install, with no entry at all, reads as not configured")
    func missingEntry() {
        #expect(ServicesMenu.state(from: nil, key: realKey) == .notConfigured)
        #expect(ServicesMenu.state(from: [:], key: realKey) == .notConfigured)
        #expect(
            ServicesMenu.state(from: ["some.other.app - X - y": [:]], key: realKey) == .notConfigured
        )
    }

    /// Shape taken verbatim from the real preferences file after ticking the
    /// box in System Settings.
    @Test("the real enabled payload reads as enabled")
    func enabledPayload() {
        let statuses: [String: Any] = [
            realKey: [
                "enabled_context_menu": 1,
                "enabled_services_menu": 1,
                "presentation_modes": ["ContextMenu": 1, "ServicesMenu": 1],
            ],
        ]
        #expect(ServicesMenu.state(from: statuses, key: realKey) == .enabled)
    }

    @Test("the context menu flag is what decides, not the services menu flag")
    func contextMenuDecides() {
        let statuses: [String: Any] = [
            realKey: [
                "enabled_context_menu": 0,
                "enabled_services_menu": 1,
                "presentation_modes": ["ContextMenu": 0, "ServicesMenu": 1],
            ],
        ]
        #expect(ServicesMenu.state(from: statuses, key: realKey) == .disabled)
    }

    @Test("the flat key is honoured when presentation_modes is absent")
    func flatFallback() {
        #expect(
            ServicesMenu.state(from: [realKey: ["enabled_context_menu": 1]], key: realKey) == .enabled
        )
        #expect(
            ServicesMenu.state(from: [realKey: ["enabled_context_menu": 0]], key: realKey) == .disabled
        )
    }

    @Test("an entry with nothing recognisable is not reported as enabled")
    func unrecognisedPayload() {
        #expect(ServicesMenu.state(from: [realKey: ["something_else": 1]], key: realKey) == .notConfigured)
    }

    @Test("only enabled counts as usable")
    func usability() {
        #expect(ServicesMenuState.enabled.isUsable)
        #expect(ServicesMenuState.disabled.isUsable == false)
        #expect(ServicesMenuState.notConfigured.isUsable == false)
    }
}
