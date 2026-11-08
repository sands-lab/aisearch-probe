package main

import ()

// DOCX pages: the second BINARY arm. A .docx is a ZIP archive of XML parts, so it is
// buildable in pure Go with archive/zip + encoding/xml (no deps). The marker goes in
// the document text so a real extractor (Word, pandoc, unzip) can read it out — the
// same fairness concern as the PDF: a malformed DOCX makes a vendor that CAN read
// Word docs look like it cannot.
//
// DETERMINISM (critical). A zip embeds a modified time per entry; left at the default
// it is time.Now(), so the bytes — and the ETag — would drift every build and the
// caching read would be meaningless. So every entry is written with a FIXED modified
// time (pageModTime) and stored uncompressed (zip.Store), making the bytes a pure
// function of the id. Realistic header (Cache-Control omitted, NOT no-store).

// The two fixed package parts. Only word/document.xml varies with the id.
const docxContentTypesXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`

const docxRelsXML = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`
