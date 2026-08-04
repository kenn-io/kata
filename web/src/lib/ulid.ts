const ENCODING = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'

function encodeTime(time: number): string {
  let value = Math.floor(time)
  let out = ''
  for (let i = 0; i < 10; i += 1) {
    out = ENCODING[value % 32] + out
    value = Math.floor(value / 32)
  }
  return out
}

export function createULID(time = Date.now()): string {
  const bytes = new Uint8Array(16)
  crypto.getRandomValues(bytes)
  let random = ''
  for (let i = 0; i < 16; i += 1) {
    random += ENCODING[bytes[i]! & 31]
  }
  return `${encodeTime(time)}${random}`
}
