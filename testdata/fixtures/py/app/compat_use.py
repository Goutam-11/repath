from .compat import clamp


def cap(value: int) -> int:
    return clamp(value, 0, 1)
