#!/usr/bin/env swift
// Text extraction only. The Python generator owns source verification and parsing.
import Foundation
import PDFKit

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

guard CommandLine.arguments.count == 2 else {
    fail("Usage: extract-swiss-pdf-text.swift PDF")
}
guard let document = PDFDocument(url: URL(fileURLWithPath: CommandLine.arguments[1])) else {
    fail("Cannot open PDF")
}
for index in 0..<document.pageCount {
    guard let text = document.page(at: index)?.string, !text.isEmpty else {
        fail("PDF page \(index + 1) has no extractable text")
    }
    print(text)
}
