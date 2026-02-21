# typed: strict
# frozen_string_literal: true

require_relative 'result'

module Core
  class Entity
    extend T::Sig
    extend T::Helpers

    abstract!

    sig { abstract.returns(Core::Result) }
    def validate; end

    sig { returns(T::Boolean) }
    def valid?
      validate.is_a?(Core::Success)
    end
  end
end
