"""Inheritance cases: diamond MRO, an inherited lookup, and super()."""


class Root:
    def ping(self) -> int:
        return 1


class Left(Root):
    def left_only(self) -> int:
        return 2

    def shared(self) -> int:
        return 4


class Right(Root):
    def right_only(self) -> int:
        return 3

    def shared(self) -> int:
        return 5


class Diamond(Left, Right):
    def go(self) -> int:
        if self.shared() > 0:
            return self.left_only()
        if self.ping() > 0:
            return self.right_only()
        return 0


class Base:
    def step(self) -> int:
        return 0


class Middle(Base):
    def step(self) -> int:
        return 1


class Leaf(Middle):
    def step(self) -> int:
        return super().step()
