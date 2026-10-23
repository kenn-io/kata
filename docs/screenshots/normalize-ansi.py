#!/usr/bin/env python3
"""Expand tmux SGR resets for Freeze 0.2.2 without changing terminal colors."""

import re
import sys


SGR_OR_NEWLINE = re.compile(r"\x1b\[([0-9;]*)m|\n")
OFF = {22: (1, 2), 23: (3,), 24: (4, 21), 25: (5, 6), 27: (7,), 28: (8,), 29: (9,)}


def normalize(text):
    attributes = {}

    def restore():
        values = [str(v) for key in sorted(attributes) for v in attributes[key]]
        return "\x1b[0m" + ("\x1b[" + ";".join(values) + "m" if values else "")

    def replace(match):
        if match[0] == "\n":
            # Freeze ends a background at each newline; the terminal may keep it.
            return "\n" + restore()
        values = [int(v or 0) for v in match[1].split(";")]
        i = 0
        while i < len(values):
            value = values[i]
            if value == 0:
                attributes.clear()
            elif value in OFF:
                for key in OFF[value]:
                    attributes.pop(key, None)
            elif value in (39, 49):
                attributes.pop(value - 1, None)
            elif value in (38, 48) and i + 1 < len(values):
                size = {5: 3, 2: 5}.get(values[i + 1], 0)
                if size and i + size <= len(values):
                    attributes[value] = tuple(values[i : i + size])
                    i += size - 1
            elif 30 <= value <= 37 or 90 <= value <= 97:
                attributes[38] = (value,)
            elif 40 <= value <= 47 or 100 <= value <= 107:
                attributes[48] = (value,)
            else:
                attributes[value] = (value,)
            i += 1
        # Freeze handles full resets, but ignores selective resets such as 39/49.
        return restore()

    return "\x1b[0m" + SGR_OR_NEWLINE.sub(replace, text)


if __name__ == "__main__":
    sys.stdout.write(normalize(sys.stdin.read()))
