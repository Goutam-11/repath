class Base:
    def total(self) -> int:
        return 1


class Node(Base):
    parent: Base
