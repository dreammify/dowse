# typed: strict
# frozen_string_literal: true

module Core
  class Result
    extend T::Sig
    extend T::Helpers

    sealed!

    sig { returns(T::Boolean) }
    def success?
      is_a?(Success)
    end

    sig { returns(T::Boolean) }
    def failure?
      is_a?(Failure)
    end
  end

  class Success < Result
    extend T::Sig

    sig { returns(NilClass) }
    def value
      nil
    end
  end

  class Failure < Result
    extend T::Sig

    sig { params(error: String).void }
    def initialize(error)
      super()
      @error = T.let(error, String)
    end

    sig { returns(String) }
    def error
      @error
    end
  end
end
