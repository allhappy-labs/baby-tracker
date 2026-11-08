import Testing
import SwiftUI
@testable import BabyUI

private typealias Scaffold = SheetFooter

@Test func footerLayoutWithoutActionsIsEmpty() {
    #expect(Scaffold.footerLayout(primary: false, secondary: false, destructive: false) == .none)
}

@Test func footerLayoutDeleteGetsOwnRow() {
    #expect(Scaffold.footerLayout(primary: true, secondary: false, destructive: true)
        == .actions(destructiveRow: true, secondary: false, primary: true))
}

@Test func footerLayoutSecondaryAndPrimary() {
    #expect(Scaffold.footerLayout(primary: true, secondary: true, destructive: false)
        == .actions(destructiveRow: false, secondary: true, primary: true))
}

@Test func headerTitleWidthReservesTheWiderSideOnBothEdges() {
    // 44 pt close, 100 pt accessory: the title gets the row less 2 × (100 + 8).
    #expect(SheetHeaderLayout.titleWidth(row: 358, leading: 44, trailing: 100, spacing: 8) == 142)
    #expect(SheetHeaderLayout.titleWidth(row: 358, leading: 100, trailing: 44, spacing: 8) == 142)
}

@Test func headerTitleWidthEmptySidesAddNoSpacing() {
    #expect(SheetHeaderLayout.titleWidth(row: 358, leading: 0, trailing: 0, spacing: 8) == 358)
}

@Test func headerTitleWidthIsNeverNegative() {
    #expect(SheetHeaderLayout.titleWidth(row: 100, leading: 80, trailing: 0, spacing: 8) == 0)
}

@Test func headerSidesAreCappedToAThirdOfTheRow() {
    #expect(SheetHeaderLayout.sideCap(row: 360) == 120)
}
