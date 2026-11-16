pub trait Describe {
    fn describe(&self) -> u32;
    fn label(&self) -> u32 {
        self.describe()
    }
}

pub trait Other {
    fn describe(&self) -> u32;
}

pub trait Sub: Describe {
    fn sub(&self) -> u32;
}

pub fn bounded<T: Describe>(value: T) -> u32 {
    value.describe()
}

pub fn where_clause<T>(value: T) -> u32
where
    T: Describe,
{
    value.describe()
}

pub fn supertrait_call<T: Sub>(value: T) -> u32 {
    value.describe()
}

pub fn ambiguous_bound<T: Describe + Other>(value: T) -> u32 {
    value.describe()
}

pub fn external_bound<T: Clone>(value: T) -> T {
    value.clone()
}

pub struct Counter {
    pub value: u32,
}

impl Counter {
    pub fn new() -> Self {
        Counter { value: 0 }
    }

    pub fn bump(&self) -> u32 {
        self.value
    }
}

pub fn make_counter() -> Counter {
    Counter::new()
}

pub fn use_return() -> u32 {
    let counter = make_counter();
    counter.bump()
}

pub fn use_chained() -> u32 {
    make_counter().bump()
}

pub trait Greet {
    fn greet(&self) -> u32 {
        7
    }
}

impl Greet for Counter {}

pub fn greet_counter(counter: &Counter) -> u32 {
    counter.greet()
}

pub trait Wave {
    fn pose(&self) -> u32 {
        1
    }
}

pub trait Flex {
    fn pose(&self) -> u32 {
        2
    }
}

impl Wave for Counter {}

impl Flex for Counter {}

pub fn pose_counter(counter: &Counter) -> u32 {
    counter.pose()
}

pub trait Buildable {
    type Made;
    fn make(&self) -> Self::Made;
}

pub struct Service2 {
    pub n: u32,
}

impl Buildable for Service2 {
    type Made = Counter;
    fn make(&self) -> Self::Made {
        Counter { value: 1 }
    }
}

pub fn via_assoc(service: &Service2) -> u32 {
    let made = service.make();
    made.bump()
}
