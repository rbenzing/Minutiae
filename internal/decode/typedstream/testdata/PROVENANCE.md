# public-sample.typedstream

An Apple NSArchiver typedstream archive of the NSString `abcdefgABCDEFG!?@[]/+*^&-_`. It holds no personal data.
It is an NSString archive, not an NSAttributedString one (the first NSString object is what `ExtractText` reads).

- Source: https://github.com/uetchy/typedstream (a general-purpose Ruby typedstream parser), file `spec/fixtures/NSString`
- Commit: `9a7fe5af785cd43c97f99e31da8f6784ba277ef5` (head of `master`, fetched 2026-10-25; the repository has no tags)
- Raw URL: `https://raw.githubusercontent.com/uetchy/typedstream/9a7fe5af785cd43c97f99e31da8f6784ba277ef5/spec/fixtures/NSString`
- How it was made (the source's `tools/archiver.swift`): `NSArchiver.archiveRootObject("abcdefgABCDEFG!?@[]/+*^&-_", toFile: "NSString")`;
  the text is the one its `spec/typedstream_spec.rb` expects.
- SHA-256 of the file: `0e5ad797a1756349aaeb12b480d23dcfa9702eef6b5ab4afc342d9a5fad2d3fb`
- Licence: MIT (the repository `LICENSE`, SHA-256 `a1b78f8dba384deda872721d816a0a2de57c3d6378a8f18525eda46f12420717`), text:

```
Copyright (c) 2015 Yasuaki Uechi <uetchy@randompaper.co>

MIT License

Permission is hereby granted, free of charge, to any person obtaining
a copy of this software and associated documentation files (the
"Software"), to deal in the Software without restriction, including
without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to
the following conditions:

The above copyright notice and this permission notice shall be
included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE
LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION
OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION
WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
```

Used by `TestExtractTextOnAPublicSample` (`internal/decode/typedstream/sample_test.go`).
