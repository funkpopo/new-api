/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact [邮箱]
*/
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { decodeCapturedContent } from '../../lib/captured-content'

function toBase64(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString('base64')
}

describe('captured usage-log content', () => {
  test('reassembles complete UTF-8 content split inside a multibyte character', () => {
    const original = 'request: 完整内容 response: ✓'
    const bytes = new TextEncoder().encode(original)
    const splitIndex = bytes.indexOf(0xe5) + 1
    const chunks = [
      toBase64(bytes.subarray(0, splitIndex)),
      toBase64(bytes.subarray(splitIndex)),
    ]

    assert.equal(
      decodeCapturedContent(chunks, 'application/json; charset=utf-8'),
      original
    )
  })

  test('preserves arbitrary binary content as a single Base64 value', () => {
    const bytes = Uint8Array.from([0, 255, 1, 254, 2, 253])
    const chunks = [
      toBase64(bytes.subarray(0, 2)),
      toBase64(bytes.subarray(2, 5)),
      toBase64(bytes.subarray(5)),
    ]

    assert.equal(decodeCapturedContent(chunks, 'audio/mpeg'), toBase64(bytes))
  })
})
