import SwiftUI
#if os(iOS)
import UIKit
#endif

/// A sheet exactly as tall as its content, that follows the content as it
/// grows and shrinks — no stated heights, no measuring round-trip.
///
/// SwiftUI's `.sheet` cannot do this on iPhone: `presentationSizing(.fitted)`
/// is ignored for height in compact width, a `CustomPresentationDetent` cannot
/// see the content, and feeding a measured height back into
/// `presentationDetents` always lags the sheet's own animation (opening at one
/// height and jumping, collapsing late, leaving a band). See
/// docs/research/2026-10-10-self-sizing-sheets.md.
///
/// So the sheet is UIKit's: a `UIHostingController` presented as a page sheet
/// whose one custom detent asks the content for its height at the sheet's
/// width. When the content changes size the detent is invalidated inside
/// `animateChanges`, and UIKit moves the sheet in step with it.
///
/// Rules for the content, all from the research's experiments:
/// - No `Form` and no `NavigationStack` at its root: both measure as zero.
///   Draw the title and Cancel in the content (`SheetScaffold`) and group rows
///   in cards. A `ScrollView` with `.scrollBounceBehavior(.basedOnSize)` is
///   fine, and scrolls once the content is taller than the screen.
/// - The keyboard is kept out of the measurement; the system raises the sheet
///   over it.
/// - The content does not inherit the presenter's custom environment values;
///   pass what it needs in `content`.
public extension View {
    func fittedSheet<Item: Identifiable, Content: View>(
        item: Binding<Item?>,
        onDismiss: (() -> Void)? = nil,
        @ViewBuilder content: @escaping (Item) -> Content
    ) -> some View {
        #if os(iOS)
        background(FittedSheetPresenter(item: item, onDismiss: onDismiss, content: content)
            .frame(width: 0, height: 0)
            .accessibilityHidden(true))
        #else
        self
        #endif
    }
}

public extension View {
    /// `fittedSheet(item:)` for a sheet with no item of its own.
    func fittedSheet<Content: View>(
        isPresented: Binding<Bool>,
        onDismiss: (() -> Void)? = nil,
        @ViewBuilder content: @escaping () -> Content
    ) -> some View {
        fittedSheet(
            item: Binding<FittedSheetToken?>(
                get: { isPresented.wrappedValue ? FittedSheetToken() : nil },
                set: { if $0 == nil { isPresented.wrappedValue = false } }
            ),
            onDismiss: onDismiss
        ) { _ in content() }
    }
}

/// The one item of an `isPresented:` fitted sheet.
public struct FittedSheetToken: Identifiable, Sendable {
    public var id: Int { 0 }
}

/// Pins its content to the top. Given a finite height it takes all of it and
/// places the content at the top, so SwiftUI never centres the content while
/// the sheet is moving to a new height; asked with no height it reports the
/// content's own — the measurement the sheet's detent uses.
private struct SheetTopFill: Layout {
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        guard let content = subviews.first else { return .zero }
        let natural = content.sizeThatFits(ProposedViewSize(width: proposal.width, height: nil))
        let width = proposal.width ?? natural.width
        if let height = proposal.height, height < 100_000 { return CGSize(width: width, height: max(height, 0)) }
        return CGSize(width: width, height: natural.height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        guard let content = subviews.first else { return }
        let natural = content.sizeThatFits(ProposedViewSize(width: bounds.width, height: nil))
        // Taller than the sheet (the largest text sizes): the content gets the
        // sheet's height, and a ScrollView in it scrolls.
        let height = min(natural.height, bounds.height)
        content.place(at: bounds.origin, anchor: .topLeading,
                      proposal: ProposedViewSize(width: bounds.width, height: height))
    }
}

#if os(iOS)
private struct FittedSheetPresenter<Item: Identifiable, Content: View>: UIViewControllerRepresentable {
    @Binding var item: Item?
    let onDismiss: (() -> Void)?
    let content: (Item) -> Content

    final class Coordinator: NSObject, UISheetPresentationControllerDelegate {
        var parent: FittedSheetPresenter
        var host: FittedSheetHost?
        var itemID: AnyHashable?

        init(_ parent: FittedSheetPresenter) { self.parent = parent }

        /// Presents once UIKit can take it. The item may already be set as
        /// the screen first appears (a link straight into "Join…?"), before
        /// the anchor is in a window, or while another sheet is still going
        /// away; presenting then is refused and nothing would try again.
        func present(_ host: FittedSheetHost, from anchor: UIViewController, attempt: Int = 0) {
            // Superseded or dismissed while waiting.
            guard self.host === host else { return }
            let ready = anchor.view.window != nil
                && anchor.presentedViewController == nil
                && anchor.transitionCoordinator == nil
            if ready {
                anchor.present(host, animated: true)
            } else if attempt < 100 {
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.05) { [weak self, weak anchor] in
                    guard let self, let anchor else { return }
                    self.present(host, from: anchor, attempt: attempt + 1)
                }
            } else {
                // Never became presentable: give the binding back.
                self.host = nil
                itemID = nil
                parent.item = nil
            }
        }

        /// Swiped down or dismissed by the system.
        func presentationControllerDidDismiss(_ presentationController: UIPresentationController) {
            host = nil
            itemID = nil
            parent.item = nil
            parent.onDismiss?()
        }
    }

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    func makeUIViewController(context: Context) -> UIViewController { UIViewController() }

    func updateUIViewController(_ anchor: UIViewController, context: Context) {
        let coordinator = context.coordinator
        coordinator.parent = self
        guard let item else {
            if let host = coordinator.host {
                coordinator.host = nil
                coordinator.itemID = nil
                if host.presentingViewController != nil {
                    host.dismiss(animated: true) { [onDismiss] in onDismiss?() }
                } else {
                    // Still waiting to be presented.
                    onDismiss?()
                }
            }
            return
        }
        let root = AnyView(SheetTopFill { content(item) })
        if let host = coordinator.host, coordinator.itemID == AnyHashable(item.id) {
            host.rootView = root
            return
        }
        // Marked before presenting, so further updates for the same item only
        // refresh its content.
        coordinator.itemID = AnyHashable(item.id)
        let old = coordinator.host
        let host = FittedSheetHost(rootView: root)
        host.sheetPresentationController?.delegate = coordinator
        coordinator.host = host
        if let old, old.presentingViewController != nil {
            // A different item replaces the sheet, as `.sheet(item:)` does.
            old.dismiss(animated: true) { coordinator.present(host, from: anchor) }
        } else {
            coordinator.present(host, from: anchor)
        }
    }
}

/// The hosting controller behind a fitted sheet: its detent is its content's
/// height at the sheet's width.
final class FittedSheetHost: UIHostingController<AnyView> {
    private var fittedHeight: CGFloat = 0

    override init(rootView: AnyView) {
        super.init(rootView: rootView)
        modalPresentationStyle = .pageSheet
        // The keyboard is not content: kept out of the measurement, the sheet
        // stays its own height and the system raises it over the keyboard.
        safeAreaRegions = .container
        // A size change of the content is what calls for a new detent.
        sizingOptions = .preferredContentSize
        // The sheet's own Liquid Glass shows through.
        view.backgroundColor = .clear
        if let sheet = sheetPresentationController {
            sheet.detents = [.custom(identifier: .init("fitted")) { [weak self] context in
                self?.height(maximum: context.maximumDetentValue)
            }]
            sheet.prefersGrabberVisible = true
        }
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }

    override var preferredContentSize: CGSize {
        didSet { if preferredContentSize != oldValue { refit() } }
    }

    override func viewDidLayoutSubviews() {
        super.viewDidLayoutSubviews()
        refit()
    }

    /// The width the sheet will have: the window's in compact width. Before
    /// the view has a window, the presenting window's.
    private var sheetWidth: CGFloat {
        if let window = view.window { return window.bounds.width }
        if let window = presentingViewController?.view.window { return window.bounds.width }
        return view.bounds.width
    }

    /// The content's height inside the sheet's safe area — the detent's unit:
    /// UIKit adds the bottom inset itself when the sheet is edge-attached.
    private func measure() -> CGFloat {
        let size = sizeThatFits(in: CGSize(width: sheetWidth, height: .greatestFiniteMagnitude))
        return (size.height - view.safeAreaInsets.top - view.safeAreaInsets.bottom).rounded(.up)
    }

    private func height(maximum: CGFloat) -> CGFloat {
        fittedHeight = measure()
        return min(max(fittedHeight, 1), maximum)
    }

    /// Never wrapped in a SwiftUI animation and never forcing layout here:
    /// that combination fed back on itself in the experiments.
    private func refit() {
        guard let sheet = sheetPresentationController, view.window != nil else { return }
        let new = measure()
        guard abs(new - fittedHeight) > 0.5 else { return }
        fittedHeight = new
        sheet.animateChanges { sheet.invalidateDetents() }
    }
}
#endif
