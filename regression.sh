#!/usr/bin/env bash
set -euo pipefail

script_dir=$(dirname -- "${BASH_SOURCE[0]}")
cd -- "$script_dir"
exec go test -run 'TestRenderSpecAddsDividerPadding|TestRenderedSizeFallsBackToReadableBytesForZeroSizedStatFile|TestSplitEditedDataPreservesTrailingNewlines|TestVirtualFileSetattrTempNoDeadlock|TestParseSpecExpandsRecursiveGlobAndSkipsEmptyPattern|TestParseSpecReturnsNoMatchesForEmptyGlobExpansion|TestParseSpecSupportsExplicitLiteralPrefix|TestBuildConcatPathPreservesWildcardSpec|TestBuildConcatPathSupportsExplicitLiteralPrefix|TestOpenSnapshotsWildcardExpansion' ./...
