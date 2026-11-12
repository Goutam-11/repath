from .util import *


def bounded(value: int) -> int:
    return clamp(value, 0, 1)
