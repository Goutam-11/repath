"""property/classmethod/staticmethod receivers and the property alias decorator."""

from .mro import Root


class Cached(Root):
    @property
    def value(self) -> int:
        return 1

    @value.setter
    def value(self, new: int) -> None:
        self.raw = new

    @staticmethod
    def helper(x: int) -> int:
        return x

    @classmethod
    def create(cls) -> "Cached":
        return cls()


def use_cached(cached: Cached) -> int:
    return cached.helper(1)


def make_cached() -> int:
    obj = Cached.create()
    return obj.helper(2)
