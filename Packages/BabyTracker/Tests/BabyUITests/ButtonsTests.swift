import Testing
import SwiftUI
@testable import BabyUI

@MainActor
private func meanAlpha(disabled: Bool) throws -> Double {
    try meanAlpha(of: Button("Go") {}.buttonStyle(.primary), disabled: disabled)
}

@MainActor
private func meanAlpha<V: View>(of button: V, disabled: Bool) throws -> Double {
    let view = button
        .disabled(disabled)
        .environment(\.colorScheme, .dark)
        .frame(width: 200, height: 54)
    let renderer = ImageRenderer(content: view)
    renderer.scale = 1
    let image = try #require(renderer.cgImage)
    var data = [UInt8](repeating: 0, count: image.width * image.height * 4)
    let context = try #require(CGContext(
        data: &data, width: image.width, height: image.height, bitsPerComponent: 8,
        bytesPerRow: image.width * 4, space: CGColorSpaceCreateDeviceRGB(),
        bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
    context.draw(image, in: CGRect(x: 0, y: 0, width: image.width, height: image.height))
    let alphas = stride(from: 3, to: data.count, by: 4).map { Double(data[$0]) }
    return alphas.reduce(0, +) / Double(alphas.count)
}

@MainActor
@Test func primaryButtonDisabledDims() throws {
    let enabled = try meanAlpha(disabled: false)
    let disabled = try meanAlpha(disabled: true)
    #expect(enabled > 100)
    #expect(disabled < enabled * 0.7)
    #expect(PrimaryButtonStyle.opacity(isEnabled: false) == 0.5)
    #expect(PrimaryButtonStyle.opacity(isEnabled: true) == 1)
}

@Test func tileFillStrength() {
    #expect(TileButtonStyle.fillStrength(isActive: true) == .strong)
    #expect(TileButtonStyle.fillStrength(isActive: false) == .soft)
}

@Test func primaryButtonHeights() {
    #expect(PrimaryButtonStyle.Size.regular.minHeight == 54)
    #expect(PrimaryButtonStyle.Size.dock.minHeight == 72)
}

@MainActor
@Test func destructiveButtonDisabledDims() throws {
    let button = DestructiveButton("Delete", identifier: "x") {}
    let enabled = try meanAlpha(of: button, disabled: false)
    let disabled = try meanAlpha(of: button, disabled: true)
    #expect(enabled > 0)
    #expect(disabled < enabled * 0.7)
}

@MainActor
@Test func circleIconButtonDisabledDims() throws {
    let button = CircleIconButton(systemImage: "plus", label: "Add", identifier: "x", isProminent: true) {}
    let enabled = try meanAlpha(of: button, disabled: false)
    let disabled = try meanAlpha(of: button, disabled: true)
    #expect(enabled > 0)
    #expect(disabled < enabled * 0.7)
}

@MainActor
@Test func selectableChipDisabledDims() throws {
    let button = Button("Chip") {}.buttonStyle(.selectableChip(Palette.feed, isSelected: true))
    let enabled = try meanAlpha(of: button, disabled: false)
    let disabled = try meanAlpha(of: button, disabled: true)
    #expect(enabled > 0)
    #expect(disabled < enabled * 0.7)
}

@Test func tileSelectedIsSolidTintWithGroundLabel() {
    #expect(TileButtonStyle.fill(tint: Palette.diaper, isActive: false, isSelected: true) == Palette.diaper)
    #expect(TileButtonStyle.fill(tint: Palette.diaper, isActive: false, isSelected: false)
        == Surface.tint(Palette.diaper, .soft))
    #expect(TileButtonStyle.labelColor(tint: Palette.diaper, isSelected: true) == Palette.ground)
    #expect(TileButtonStyle.labelColor(tint: Palette.diaper, isSelected: false) == Palette.diaper)
}

@Test func widgetTilesKeepLittleSidePadding() {
    #expect(TileButtonStyle.Size.widget.horizontalPadding == Spacing.xs)
    #expect(TileButtonStyle.Size.regular.horizontalPadding == Spacing.m)
    #expect(TileButtonStyle.Size.large.horizontalPadding == Spacing.m)
}
