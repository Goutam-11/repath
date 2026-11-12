from ..util import clamp


def run(value: int) -> int:
    return clamp(value, 0, 5)
